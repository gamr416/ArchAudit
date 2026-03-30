package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	URLs          []string
	Duration      time.Duration
	Workers       int
	Timeout       time.Duration
	MaxRPS        int
	PrintInterval time.Duration
	SLAThreshold  time.Duration
	SLAExplicit   bool
	ExploreStep   int
	Resources1    int
	Resources2    int
	StabilityErr  float64
}

type Phase int

const (
	PhaseExplore Phase = iota
	PhaseStress
	PhaseRecovery
	PhaseComplete
)

type TestPhaseConfig struct {
	Name        string
	Duration    time.Duration
	TargetRPS   int
	Workers     int
	Description string
}

type Job struct {
	URL       string
	StartTime time.Time
	Phase     Phase
}

type Result struct {
	Success    bool
	Latency    time.Duration
	Bytes      int64
	URL        string
	StatusCode int
	Phase      Phase
	Timestamp  time.Time
}

type Stats struct {
	TotalRequests    int64
	SuccessfulReqs   int64
	FailedReqs       int64
	TotalBytes       int64
	Latencies        []time.Duration
	LatenciesMu      sync.Mutex
	StatusCodes      map[int]int64
	PhaseStats       map[Phase]*PhaseStats
	mu               sync.RWMutex
	SLAViolationCnt  int64
	SamplesForP95    int
	ErrorRate        float64
	SLAViolationRate float64
}

type PhaseStats struct {
	StartTime        time.Time
	EndTime          time.Time
	TotalReqs        int64
	SuccessReqs      int64
	FailedReqs       int64
	AvgLatency       time.Duration
	P95Latency       time.Duration
	MinLatencyNs     int64
	MaxLatencyNs     int64
	MaxRPS           float64
	SLAViolations    int64
	SLAViolationRate float64
	ErrorRate        float64
	Latencies        []time.Duration
	LatenciesMu      sync.Mutex
	IsStable         bool
	StabilityTime    time.Time
}

func (ps *PhaseStats) GetMinLatency() time.Duration {
	return time.Duration(atomic.LoadInt64(&ps.MinLatencyNs))
}

func (ps *PhaseStats) GetMaxLatency() time.Duration {
	return time.Duration(atomic.LoadInt64(&ps.MaxLatencyNs))
}

func (ps *PhaseStats) SetMinLatency(v int64) {
	atomic.StoreInt64(&ps.MinLatencyNs, v)
}

func (ps *PhaseStats) SetMaxLatency(v int64) {
	atomic.StoreInt64(&ps.MaxLatencyNs, v)
}

func NewStats() *Stats {
	return &Stats{
		StatusCodes: make(map[int]int64),
		PhaseStats: map[Phase]*PhaseStats{
			PhaseExplore:  NewPhaseStats(),
			PhaseStress:   NewPhaseStats(),
			PhaseRecovery: NewPhaseStats(),
		},
		SamplesForP95: 10000,
	}
}

func NewPhaseStats() *PhaseStats {
	return &PhaseStats{
		MinLatencyNs: math.MaxInt64,
	}
}

func (s *Stats) Record(result Result, slaThreshold time.Duration) {
	atomic.AddInt64(&s.TotalRequests, 1)
	if result.Success {
		atomic.AddInt64(&s.SuccessfulReqs, 1)
	} else {
		atomic.AddInt64(&s.FailedReqs, 1)
	}
	atomic.AddInt64(&s.TotalBytes, result.Bytes)

	s.mu.Lock()
	s.StatusCodes[result.StatusCode]++
	s.mu.Unlock()

	ps := s.PhaseStats[result.Phase]
	if ps == nil {
		s.mu.Lock()
		s.PhaseStats[result.Phase] = NewPhaseStats()
		ps = s.PhaseStats[result.Phase]
		s.mu.Unlock()
	}

	atomic.AddInt64(&ps.TotalReqs, 1)
	if result.Success {
		atomic.AddInt64(&ps.SuccessReqs, 1)
	} else {
		atomic.AddInt64(&ps.FailedReqs, 1)
	}

	ps.LatenciesMu.Lock()
	ps.Latencies = append(ps.Latencies, result.Latency)
	if len(ps.Latencies) > s.SamplesForP95 {
		ps.Latencies = ps.Latencies[len(ps.Latencies)-s.SamplesForP95:]
	}
	ps.LatenciesMu.Unlock()

	latencyNs := int64(result.Latency)
	for {
		oldMin := atomic.LoadInt64(&ps.MinLatencyNs)
		if latencyNs < oldMin {
			if atomic.CompareAndSwapInt64(&ps.MinLatencyNs, oldMin, latencyNs) {
				break
			}
		} else {
			break
		}
	}
	for {
		oldMax := atomic.LoadInt64(&ps.MaxLatencyNs)
		if latencyNs > oldMax {
			if atomic.CompareAndSwapInt64(&ps.MaxLatencyNs, oldMax, latencyNs) {
				break
			}
		} else {
			break
		}
	}

	if result.Latency > slaThreshold {
		atomic.AddInt64(&s.SLAViolationCnt, 1)
		atomic.AddInt64(&ps.SLAViolations, 1)
	}
}

func (s *Stats) GetP95Latency() time.Duration {
	latencies := s.collectAllLatencies()
	if len(latencies) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)) * 0.95)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	if idx < 0 {
		return 0
	}
	return sorted[idx]
}

func (s *Stats) collectAllLatencies() []time.Duration {
	var all []time.Duration
	s.mu.RLock()
	for phase, ps := range s.PhaseStats {
		if phase == PhaseComplete {
			continue
		}
		ps.LatenciesMu.Lock()
		all = append(all, ps.Latencies...)
		ps.LatenciesMu.Unlock()
	}
	s.mu.RUnlock()
	return all
}

func (s *Stats) GetRPS(elapsed time.Duration) float64 {
	total := atomic.LoadInt64(&s.TotalRequests)
	if total == 0 {
		return 0
	}
	return float64(total) / elapsed.Seconds()
}

type PhaseResult struct {
	StartTime        time.Time
	EndTime          time.Time
	TotalReqs        int64
	SuccessReqs      int64
	FailedReqs       int64
	AvgLatency       time.Duration
	P95Latency       time.Duration
	MinLatency       time.Duration
	MaxLatency       time.Duration
	MaxRPS           float64
	SLAViolations    int64
	SLAViolationRate float64
	ErrorRate        float64
}

func (s *Stats) CalculatePhaseStats(phase Phase) PhaseResult {
	s.mu.RLock()
	ps := s.PhaseStats[phase]
	s.mu.RUnlock()

	if ps == nil {
		return PhaseResult{}
	}

	totalReqs := atomic.LoadInt64(&ps.TotalReqs)
	failedReqs := atomic.LoadInt64(&ps.FailedReqs)
	slaViolations := atomic.LoadInt64(&ps.SLAViolations)

	result := PhaseResult{
		TotalReqs:     totalReqs,
		SuccessReqs:   atomic.LoadInt64(&ps.SuccessReqs),
		FailedReqs:    failedReqs,
		SLAViolations: slaViolations,
		MinLatency:    ps.GetMinLatency(),
		MaxLatency:    ps.GetMaxLatency(),
	}

	if totalReqs > 0 {
		result.ErrorRate = float64(failedReqs) / float64(totalReqs)
		result.SLAViolationRate = float64(slaViolations) / float64(totalReqs)
	}

	ps.LatenciesMu.Lock()
	latencies := make([]time.Duration, len(ps.Latencies))
	copy(latencies, ps.Latencies)
	ps.LatenciesMu.Unlock()

	if len(latencies) > 0 && result.TotalReqs > 0 {
		var totalLatency int64
		for _, l := range latencies {
			totalLatency += int64(l)
		}
		result.AvgLatency = time.Duration(totalLatency / int64(len(latencies)))

		sorted := make([]time.Duration, len(latencies))
		copy(sorted, latencies)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		idx := int(float64(len(sorted)) * 0.95)
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		if idx >= 0 {
			result.P95Latency = sorted[idx]
		}
	}

	if result.MinLatency == time.Hour {
		result.MinLatency = 0
	}

	if !ps.StartTime.IsZero() && !ps.EndTime.IsZero() {
		duration := ps.EndTime.Sub(ps.StartTime)
		if duration > 0 {
			result.MaxRPS = float64(result.TotalReqs) / duration.Seconds()
		}
		result.StartTime = ps.StartTime
		result.EndTime = ps.EndTime
	}

	return result
}

func (s *Stats) SetPhaseTime(phase Phase, start, end time.Time) {
	s.mu.Lock()
	if ps, ok := s.PhaseStats[phase]; ok {
		ps.StartTime = start
		ps.EndTime = end
	}
	s.mu.Unlock()
}

func (s *Stats) Print(elapsed time.Duration, config *Config) {
	total := atomic.LoadInt64(&s.TotalRequests)
	success := atomic.LoadInt64(&s.SuccessfulReqs)
	failed := atomic.LoadInt64(&s.FailedReqs)
	bytes := atomic.LoadInt64(&s.TotalBytes)
	slaViolations := atomic.LoadInt64(&s.SLAViolationCnt)

	p95 := s.GetP95Latency()
	rps := s.GetRPS(elapsed)
	successRate := float64(success) / float64(total) * 100

	fmt.Println("\n=== TEST SUMMARY ===")
	fmt.Printf("Total: %d | RPS: %.0f | Time: %v\n", total, rps, elapsed.Round(time.Second))
	fmt.Printf("Success: %d | Failed: %d | Data: %.1fKB\n", success, failed, float64(bytes)/1024)
	fmt.Printf("Success Rate: %.2f%% | Error Rate: %.2f%%\n", successRate, 100-successRate)
	fmt.Printf("P95 Latency: %v | SLA Violations: %.2f%% (%d)\n", p95, float64(slaViolations)/float64(total)*100, slaViolations)

	fmt.Println("\n=== PHASE BREAKDOWN ===")
	for phase := Phase(0); phase < PhaseComplete; phase++ {
		ps := s.CalculatePhaseStats(phase)
		if ps.TotalReqs == 0 {
			continue
		}
		phaseName := []string{"EXPLORE", "STRESS", "RECOVERY"}[phase]
		fmt.Printf("%s: %d reqs | RPS: %.0f | P95: %v\n", phaseName, ps.TotalReqs, ps.MaxRPS, ps.P95Latency)
	}
}

type AnalyticalModule struct {
	stats            *Stats
	slaThreshold     time.Duration
	bottleneck       string
	scalingEff       float64
	recoveryTime     time.Duration
	errorRate        float64
	p95Latency       time.Duration
	slaViolationRate float64
}

func NewAnalyticalModule(stats *Stats, slaThreshold time.Duration, resources1, resources2 int, stabilityErrThreshold float64) *AnalyticalModule {
	return &AnalyticalModule{
		stats:        stats,
		slaThreshold: slaThreshold,
	}
}

func (am *AnalyticalModule) Analyze(resources1, resources2 int, stabilityErrThreshold float64) string {
	am.detectBottleneck()
	am.calculateScalingEfficiency(resources1, resources2)
	am.estimateRecoveryTime(am.slaThreshold, stabilityErrThreshold)

	stress := am.stats.CalculatePhaseStats(PhaseStress)
	am.errorRate = stress.ErrorRate
	am.p95Latency = stress.P95Latency
	am.slaViolationRate = stress.SLAViolationRate

	var analysis strings.Builder

	analysis.WriteString("\n=== ARCHITECTURAL METRICS ===\n")

	scalingStatus := "good"
	if am.scalingEff >= 0.9 {
		scalingStatus = "excellent"
	} else if am.scalingEff < 0.7 {
		scalingStatus = "POOR"
	}
	analysis.WriteString(fmt.Sprintf("Scaling Efficiency: %.1f%% [%s]\n", am.scalingEff*100, scalingStatus))

	errStatus := "good"
	if am.errorRate < 0.01 {
		errStatus = "excellent"
	} else if am.errorRate > 0.10 {
		errStatus = "CRITICAL"
	} else if am.errorRate > 0.05 {
		errStatus = "high"
	}
	analysis.WriteString(fmt.Sprintf("Error Rate: %.2f%% [%s]\n", am.errorRate*100, errStatus))

	p95Status := "good"
	if am.p95Latency < 200*time.Millisecond {
		p95Status = "excellent"
	} else if am.p95Latency > 1*time.Second {
		p95Status = "CRITICAL"
	} else if am.p95Latency > 500*time.Millisecond {
		p95Status = "high"
	}
	analysis.WriteString(fmt.Sprintf("P95 Latency: %v [%s]\n", am.p95Latency, p95Status))

	slaStatus := "good"
	if am.slaViolationRate < 0.01 {
		slaStatus = "excellent"
	} else if am.slaViolationRate > 0.05 {
		slaStatus = "CRITICAL"
	} else if am.slaViolationRate > 0.03 {
		slaStatus = "high"
	}
	analysis.WriteString(fmt.Sprintf("SLA Violations: %.2f%% [%s]\n", am.slaViolationRate*100, slaStatus))

	recStatus := "good"
	if am.recoveryTime < 30*time.Second {
		recStatus = "excellent"
	} else if am.recoveryTime > 120*time.Second {
		recStatus = "SLOW"
	} else if am.recoveryTime > 30*time.Second {
		recStatus = "normal"
	}
	analysis.WriteString(fmt.Sprintf("Recovery Time: %v [%s]\n", am.recoveryTime.Round(time.Second), recStatus))

	analysis.WriteString("\n")
	if am.bottleneck != "None" {
		analysis.WriteString(fmt.Sprintf("Bottleneck: %s\n", am.bottleneck))
		analysis.WriteString(fmt.Sprintf("Action: Optimize %s\n", am.bottleneck))
	} else {
		analysis.WriteString("Bottleneck: None detected\n")
	}

	return analysis.String()
}

func (am *AnalyticalModule) detectBottleneck() {
	stress := am.stats.CalculatePhaseStats(PhaseStress)
	explore := am.stats.CalculatePhaseStats(PhaseExplore)

	if stress.TotalReqs == 0 {
		am.bottleneck = "No Data"
		return
	}

	switch {
	case stress.ErrorRate > 0.10:
		am.bottleneck = "Critical Error Rate"
	case stress.ErrorRate > 0.05:
		am.bottleneck = "High Error Rate"
	case stress.P95Latency > 1*time.Second:
		am.bottleneck = "Critical P95 Latency"
	case stress.P95Latency > 500*time.Millisecond:
		am.bottleneck = "High P95 Latency"
	case stress.AvgLatency > 500*time.Millisecond:
		am.bottleneck = "Slow Response"
	case stress.SLAViolationRate > 0.05:
		am.bottleneck = "SLA Violations"
	case stress.MaxRPS < 1000 && stress.TotalReqs > 1000:
		am.bottleneck = "Low Throughput"
	case explore.TotalReqs > 0 && stress.MaxRPS < explore.MaxRPS*1.5:
		am.bottleneck = "Poor Scaling"
	default:
		am.bottleneck = "None"
	}
}

func (am *AnalyticalModule) calculateScalingEfficiency(resources1, resources2 int) {
	explore := am.stats.CalculatePhaseStats(PhaseExplore)
	stress := am.stats.CalculatePhaseStats(PhaseStress)

	if explore.TotalReqs == 0 || stress.TotalReqs == 0 {
		am.scalingEff = 0
		return
	}

	stressDuration := stress.EndTime.Sub(stress.StartTime)
	exploreDuration := explore.EndTime.Sub(explore.StartTime)

	if stressDuration <= 0 || exploreDuration <= 0 {
		am.scalingEff = 0
		return
	}

	rps1 := float64(explore.TotalReqs) / exploreDuration.Seconds()
	rps2 := float64(stress.TotalReqs) / stressDuration.Seconds()

	if rps1 <= 0 {
		am.scalingEff = 0
		return
	}

	if resources1 <= 0 {
		resources1 = 1
	}
	if resources2 <= 0 {
		resources2 = resources1 * 4
	}

	rpsRatio := rps2 / rps1
	resourceRatio := float64(resources2) / float64(resources1)
	am.scalingEff = rpsRatio / resourceRatio
}

func (am *AnalyticalModule) estimateRecoveryTime(slaThreshold time.Duration, stabilityErrThreshold float64) {
	recovery := am.stats.CalculatePhaseStats(PhaseRecovery)
	if recovery.TotalReqs == 0 {
		am.recoveryTime = 0
		return
	}

	isStable := recovery.ErrorRate < stabilityErrThreshold && recovery.P95Latency < slaThreshold
	if isStable {
		am.recoveryTime = recovery.EndTime.Sub(recovery.StartTime)
	} else {
		am.recoveryTime = recovery.EndTime.Sub(recovery.StartTime)
	}
}

func worker(id int, ctx context.Context, client *http.Client, jobs <-chan Job, results chan<- Result, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-jobs:
			if !ok {
				return
			}
			result := makeRequest(client, job.URL)
			result.Phase = job.Phase
			result.Timestamp = time.Now()
			results <- result
		}
	}
}

func makeRequest(client *http.Client, url string) Result {
	start := time.Now()
	resp, err := client.Get(url)
	latency := time.Since(start)

	var bytes int64
	var success bool
	statusCode := 0

	if err != nil {
		success = false
	} else {
		statusCode = resp.StatusCode
		bytesRead, _ := io.Copy(io.Discard, resp.Body)
		bytes = bytesRead
		resp.Body.Close()
		success = resp.StatusCode >= 200 && resp.StatusCode < 300
	}

	return Result{
		Success:    success,
		Latency:    latency,
		Bytes:      bytes,
		URL:        url,
		StatusCode: statusCode,
	}
}

func loadURLsFromFile(filepath string) ([]string, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var urls []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		url := strings.TrimSpace(scanner.Text())
		if url != "" && !strings.HasPrefix(url, "#") {
			urls = append(urls, url)
		}
	}
	return urls, scanner.Err()
}

func adaptiveWorkerCount(targetRPS int) int {
	if targetRPS <= 0 {
		return 100
	}
	if targetRPS <= 100 {
		return int(math.Max(10, float64(targetRPS)*1.5))
	}
	if targetRPS <= 1000 {
		return int(math.Min(200, float64(targetRPS)*0.8))
	}
	return int(math.Min(500, float64(targetRPS)*0.5))
}

func runPhase(ctx context.Context, client *http.Client, config *Config, phase Phase, phaseConfig TestPhaseConfig, stats *Stats) {
	workers := phaseConfig.Workers
	if workers <= 0 {
		workers = adaptiveWorkerCount(phaseConfig.TargetRPS)
	}

	jobs := make(chan Job, workers*10)
	results := make(chan Result, workers*10)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker(i, ctx, client, jobs, results, &wg)
	}

	var resultWg sync.WaitGroup
	resultWg.Add(1)
	slaThreshold := config.SLAThreshold
	go func() {
		defer resultWg.Done()
		for result := range results {
			stats.Record(result, slaThreshold)
		}
	}()

	fmt.Printf("\n=== %s Phase ===\n", phaseConfig.Name)
	fmt.Printf("Duration: %v | Target RPS: %d | Workers: %d\n", phaseConfig.Duration, phaseConfig.TargetRPS, workers)

	startTime := time.Now()
	phaseCtx, cancel := context.WithTimeout(ctx, phaseConfig.Duration)
	defer cancel()

	urlIdx := 0

	if phaseConfig.TargetRPS > 0 {
		interval := time.Second / time.Duration(phaseConfig.TargetRPS)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-phaseCtx.Done():
				goto done
			case <-ticker.C:
				job := Job{
					URL:       config.URLs[urlIdx%len(config.URLs)],
					StartTime: time.Now(),
					Phase:     phase,
				}
				select {
				case jobs <- job:
					urlIdx++
				default:
				}
			}
		}
	} else {
		ticker := time.NewTicker(1 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-phaseCtx.Done():
				goto done
			case <-ticker.C:
				job := Job{
					URL:       config.URLs[urlIdx%len(config.URLs)],
					StartTime: time.Now(),
					Phase:     phase,
				}
				select {
				case jobs <- job:
					urlIdx++
				default:
				}
			}
		}
	}
done:
	stats.SetPhaseTime(phase, startTime, time.Now())
	close(jobs)
	wg.Wait()
	close(results)
	resultWg.Wait()

	ps := stats.CalculatePhaseStats(phase)
	fmt.Printf("--- %s done: %d reqs | RPS: %.0f | P95: %v\n",
		phaseConfig.Name, ps.TotalReqs, ps.MaxRPS, ps.P95Latency)
}

func main() {
	var (
		urlsFlag         = flag.String("urls", "", "Comma-separated list of URLs to test")
		urlsFileFlag     = flag.String("urls-file", "", "File containing URLs (one per line)")
		durationFlag     = flag.Duration("duration", 2*time.Minute, "Total test duration")
		workersFlag      = flag.Int("workers", 0, "Number of workers (0 = auto-adaptive)")
		timeoutFlag      = flag.Duration("timeout", 10*time.Second, "HTTP client timeout")
		maxRPSFlag       = flag.Int("max-rps", 0, "Maximum RPS for stress phase (0 = unlimited)")
		intervalFlag     = flag.Duration("print-interval", 5*time.Second, "Stats print interval")
		slaFlag          = flag.Duration("sla", 200*time.Millisecond, "SLA threshold for latency")
		exploreFlag      = flag.Int("explore-step", 4, "Load multiplier for exploration phase")
		resources1Flag   = flag.Int("resources1", 1, "Resources in explore phase (e.g., 1 server)")
		resources2Flag   = flag.Int("resources2", 4, "Resources in stress phase (e.g., 4 servers)")
		stabilityErrFlag = flag.Float64("stability-err", 0.05, "Error rate threshold for stability (0.05 = 5%)")
	)
	flag.Parse()

	slaExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "sla" {
			slaExplicit = true
		}
	})

	config := Config{
		Duration:      *durationFlag,
		Workers:       *workersFlag,
		Timeout:       *timeoutFlag,
		MaxRPS:        *maxRPSFlag,
		PrintInterval: *intervalFlag,
		SLAThreshold:  *slaFlag,
		SLAExplicit:   slaExplicit,
		ExploreStep:   *exploreFlag,
		Resources1:    *resources1Flag,
		Resources2:    *resources2Flag,
		StabilityErr:  *stabilityErrFlag,
	}

	if *urlsFlag != "" {
		config.URLs = parseURLs(*urlsFlag)
	} else if *urlsFileFlag != "" {
		urls, err := loadURLsFromFile(*urlsFileFlag)
		if err != nil {
			fmt.Printf("Error loading URLs from file: %v\n", err)
			os.Exit(1)
		}
		config.URLs = urls
	} else {
		fmt.Println("Error: Must specify either -urls or -urls-file")
		flag.PrintDefaults()
		os.Exit(1)
	}

	fmt.Println("=== ArchAudit ===")
	if config.SLAExplicit {
		fmt.Printf("URLs: %d | Duration: %v | SLA: %v | Target RPS: %d\n", len(config.URLs), config.Duration, config.SLAThreshold, config.MaxRPS)
	} else {
		fmt.Printf("URLs: %d | Duration: %v | Target RPS: %d\n", len(config.URLs), config.Duration, config.MaxRPS)
	}
	fmt.Printf("Resources: %d -> %d | Stability Error: %.0f%%\n", config.Resources1, config.Resources2, config.StabilityErr*100)
	fmt.Println()

	client := &http.Client{
		Timeout: config.Timeout,
		Transport: &http.Transport{
			MaxIdleConns:        10000,
			MaxIdleConnsPerHost: 5000,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}

	stats := NewStats()
	startTime := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), config.Duration)
	defer cancel()

	phases := getPhases(config)

	for _, pc := range phases {
		phase := getPhaseFromName(pc.Name)
		runPhase(ctx, client, &config, phase, pc, stats)
		select {
		case <-ctx.Done():
			break
		default:
		}
	}

	elapsed := time.Since(startTime)
	stats.Print(elapsed, &config)

	analytics := NewAnalyticalModule(stats, config.SLAThreshold, config.Resources1, config.Resources2, config.StabilityErr)
	fmt.Print(analytics.Analyze(config.Resources1, config.Resources2, config.StabilityErr))
}

func getPhaseFromName(name string) Phase {
	switch name {
	case "EXPLORE":
		return PhaseExplore
	case "STRESS":
		return PhaseStress
	case "RECOVERY":
		return PhaseRecovery
	default:
		return PhaseComplete
	}
}

func getPhases(config Config) []TestPhaseConfig {
	totalDur := config.Duration

	recoveryDur := time.Duration(float64(totalDur) * 0.15)
	exploreDur := time.Duration(float64(totalDur) * 0.25)
	stressDur := totalDur - exploreDur - recoveryDur

	if stressDur < 10*time.Second && totalDur >= 30*time.Second {
		stressDur = 10 * time.Second
		exploreDur = (totalDur - stressDur - recoveryDur) / 2
	}

	baseRPS := config.MaxRPS
	if baseRPS == 0 {
		baseRPS = 1000
	}

	exploreRPS := baseRPS / config.ExploreStep
	if exploreRPS < 10 {
		exploreRPS = 10
	}

	return []TestPhaseConfig{
		{
			Name:        "EXPLORE",
			Duration:    exploreDur,
			TargetRPS:   exploreRPS,
			Workers:     0,
			Description: "Determining system limits with incremental load",
		},
		{
			Name:        "STRESS",
			Duration:    stressDur,
			TargetRPS:   baseRPS,
			Workers:     0,
			Description: "Sustained peak load to find breaking point",
		},
		{
			Name:        "RECOVERY",
			Duration:    recoveryDur,
			TargetRPS:   baseRPS / 10,
			Workers:     0,
			Description: "Measuring system recovery time",
		},
	}
}

func parseURLs(urlsStr string) []string {
	var urls []string
	parts := strings.Split(urlsStr, ",")
	for _, part := range parts {
		url := strings.TrimSpace(part)
		if url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}
