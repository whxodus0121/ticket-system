package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type requestResult struct {
	requestID   int64
	userID      string
	status      int
	errorType   string
	latency     time.Duration
	startedAt   time.Time
	completedAt time.Time
	rawError    string
}

type summary struct {
	mu              sync.Mutex
	totalAttempts   int64
	transportErrors int64
	statusCounts    map[int]int64
	latenciesMs     []float64
}

func (s *summary) add(result requestResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalAttempts++
	s.latenciesMs = append(s.latenciesMs, float64(result.latency.Microseconds())/1000)
	if result.rawError != "" {
		s.transportErrors++
		return
	}
	s.statusCounts[result.status]++
}

func classifyErr(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "TIMEOUT"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "connection refused") || strings.Contains(message, "actively refused"):
		return "CONN_REFUSED"
	case strings.Contains(message, "connection reset"):
		return "CONN_RESET"
	case strings.Contains(message, "no free ports"),
		strings.Contains(message, "cannot assign requested address"),
		strings.Contains(message, "only one usage of each socket address"):
		return "PORT_EXHAUSTED"
	case errors.Is(err, io.EOF) || strings.Contains(message, "eof"):
		return "EOF"
	default:
		return "OTHER"
	}
}

func waitForServer(client *http.Client, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if err == nil {
			response, requestErr := client.Do(request)
			if requestErr == nil {
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				cancel()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			} else {
				cancel()
			}
		} else {
			cancel()
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("API did not become ready within %s", timeout)
}

func main() {
	totalUsers := flag.Int("requests", 50000, "number of user purchase journeys")
	maxConcurrent := flag.Int("concurrency", 300, "maximum concurrent user journeys")
	baseURL := flag.String("base-url", "http://127.0.0.1:8080", "ticket API base URL")
	readyTimeout := flag.Duration("ready-timeout", 30*time.Second, "API readiness timeout")
	rampUp := flag.Duration("ramp-up", 2*time.Second, "time used to establish the initial concurrent connections")
	flag.Parse()
	if *totalUsers <= 0 || *maxConcurrent <= 0 {
		log.Fatal("requests and concurrency must be positive")
	}

	transport := &http.Transport{
		MaxIdleConns:        *maxConcurrent,
		MaxIdleConnsPerHost: *maxConcurrent,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	if err := waitForServer(client, *baseURL, *readyTimeout); err != nil {
		log.Fatal(err)
	}

	resultFile, err := os.Create("test_log.csv")
	if err != nil {
		log.Fatal(err)
	}
	defer resultFile.Close()
	errorFile, err := os.Create("test_errors.csv")
	if err != nil {
		log.Fatal(err)
	}
	defer errorFile.Close()

	resultBuffer := bufio.NewWriter(resultFile)
	errorBuffer := bufio.NewWriter(errorFile)
	resultWriter := csv.NewWriter(resultBuffer)
	errorWriter := csv.NewWriter(errorBuffer)
	header := []string{"request_id", "user_id", "http_status", "error_type", "latency_ms", "started_at", "completed_at", "error"}
	if err := resultWriter.Write(header); err != nil {
		log.Fatal(err)
	}
	if err := errorWriter.Write(header); err != nil {
		log.Fatal(err)
	}

	var writeMu sync.Mutex
	stats := &summary{statusCounts: map[int]int64{}}
	var requestSequence int64
	var completedUsers int64

	record := func(result requestResult) {
		stats.add(result)
		row := []string{
			strconv.FormatInt(result.requestID, 10),
			result.userID,
			strconv.Itoa(result.status),
			result.errorType,
			fmt.Sprintf("%.3f", float64(result.latency.Microseconds())/1000),
			result.startedAt.UTC().Format(time.RFC3339Nano),
			result.completedAt.UTC().Format(time.RFC3339Nano),
			result.rawError,
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := resultWriter.Write(row); err != nil {
			log.Printf("write result CSV: %v", err)
		}
		if result.rawError != "" {
			if err := errorWriter.Write(row); err != nil {
				log.Printf("write error CSV: %v", err)
			}
		}
	}

	start := time.Now()
	stopHeartbeat := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fmt.Printf("[progress] users=%d/%d elapsed=%.1fs\n", atomic.LoadInt64(&completedUsers), *totalUsers, time.Since(start).Seconds())
			case <-stopHeartbeat:
				return
			}
		}
	}()

	semaphore := make(chan struct{}, *maxConcurrent)
	var wg sync.WaitGroup
	for user := 0; user < *totalUsers; user++ {
		wg.Add(1)
		go func(user int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			defer atomic.AddInt64(&completedUsers, 1)

			userID := fmt.Sprintf("user_%d", user)
			url := fmt.Sprintf("%s/ticket?user_id=%s", *baseURL, userID)
			for {
				requestID := atomic.AddInt64(&requestSequence, 1)
				startedAt := time.Now()
				response, requestErr := client.Get(url)
				completedAt := time.Now()
				result := requestResult{
					requestID:   requestID,
					userID:      userID,
					latency:     completedAt.Sub(startedAt),
					startedAt:   startedAt,
					completedAt: completedAt,
				}
				if requestErr != nil {
					result.errorType = classifyErr(requestErr)
					result.rawError = requestErr.Error()
					record(result)
					return
				}

				result.status = response.StatusCode
				if response.StatusCode == http.StatusAccepted {
					var payload map[string]interface{}
					_ = json.NewDecoder(response.Body).Decode(&payload)
					_, _ = io.Copy(io.Discard, response.Body)
				} else {
					_, _ = io.Copy(io.Discard, response.Body)
				}
				response.Body.Close()
				record(result)
				if response.StatusCode != http.StatusAccepted {
					return
				}
				time.Sleep(time.Second)
			}
		}(user)
		if user < *maxConcurrent && *rampUp > 0 {
			time.Sleep(*rampUp / time.Duration(*maxConcurrent))
		}
	}
	wg.Wait()
	close(stopHeartbeat)
	duration := time.Since(start)

	resultWriter.Flush()
	errorWriter.Flush()
	if err := resultWriter.Error(); err != nil {
		log.Fatal(err)
	}
	if err := errorWriter.Error(); err != nil {
		log.Fatal(err)
	}
	if err := resultBuffer.Flush(); err != nil {
		log.Fatal(err)
	}
	if err := errorBuffer.Flush(); err != nil {
		log.Fatal(err)
	}

	stats.mu.Lock()
	sort.Float64s(stats.latenciesMs)
	latencies := append([]float64(nil), stats.latenciesMs...)
	totalAttempts := stats.totalAttempts
	transportErrors := stats.transportErrors
	statusCounts := make(map[int]int64, len(stats.statusCounts))
	for status, count := range stats.statusCounts {
		statusCounts[status] = count
	}
	stats.mu.Unlock()

	percentile := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		index := int(float64(len(latencies)-1) * p)
		return latencies[index]
	}
	var latencyTotal float64
	for _, latency := range latencies {
		latencyTotal += latency
	}
	httpResponses := totalAttempts - transportErrors
	var successfulRequests int64
	for status, count := range statusCounts {
		if status >= 200 && status < 300 {
			successfulRequests += count
		}
	}
	failedRequests := totalAttempts - successfulRequests
	successRate := 0.0
	if totalAttempts > 0 {
		successRate = float64(successfulRequests) / float64(totalAttempts) * 100
	}

	fmt.Println("\n========== load test result ==========")
	fmt.Printf("configured users : %d\n", *totalUsers)
	fmt.Printf("concurrency      : %d\n", *maxConcurrent)
	fmt.Printf("initial ramp-up  : %s\n", *rampUp)
	fmt.Printf("HTTP attempts    : %d\n", totalAttempts)
	fmt.Printf("HTTP responses   : %d\n", httpResponses)
	fmt.Printf("successful (2xx) : %d\n", successfulRequests)
	fmt.Printf("failed (non-2xx) : %d\n", failedRequests)
	fmt.Printf("transport errors : %d\n", transportErrors)
	fmt.Printf("HTTP success rate: %.2f%%\n", successRate)
	fmt.Printf("duration         : %.3fs\n", duration.Seconds())
	fmt.Printf("throughput       : %.2f req/s\n", float64(totalAttempts)/duration.Seconds())
	fmt.Println("status counts:")
	statuses := make([]int, 0, len(statusCounts))
	for status := range statusCounts {
		statuses = append(statuses, status)
	}
	sort.Ints(statuses)
	for _, status := range statuses {
		fmt.Printf("  %d: %d\n", status, statusCounts[status])
	}
	if len(latencies) > 0 {
		fmt.Printf("latency avg/p50/p95/p99: %.3f / %.3f / %.3f / %.3f ms\n",
			latencyTotal/float64(len(latencies)), percentile(.50), percentile(.95), percentile(.99))
	}
	fmt.Println("CSV: test_log.csv, test_errors.csv")
}
