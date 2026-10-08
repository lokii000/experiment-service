// Standalone, dependency-free HTTP assignment load test.
// Run from the repository root:
//
//	go run ./loadtest/assignment.go -rps 100 -duration 30s -workers 40
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type assignmentResponse struct {
	Assignments []struct {
		ExperimentKey string `json:"experiment_key"`
		Status        string `json:"status"`
		VariantKey    string `json:"variant_key"`
	} `json:"assignments"`
}

type stats struct {
	mu           sync.Mutex
	latenciesMS  []float64 // successful requests only
	statusCounts map[int]int
	errors       map[string]int
	completed    int
	successful   int
}

func (s *stats) add(status int, latency time.Duration, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed++
	if status != 0 {
		s.statusCounts[status]++
	}
	if reason != "" {
		s.errors[reason]++
		return
	}
	s.successful++
	s.latenciesMS = append(s.latenciesMS, float64(latency)/float64(time.Millisecond))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

func main() {
	rps := flag.Int("rps", 100, "target new requests per second")
	duration := flag.Duration("duration", 30*time.Second, "test duration")
	workers := flag.Int("workers", 50, "maximum concurrent HTTP workers")
	timeout := flag.Duration("timeout", 2*time.Second, "per-request HTTP timeout")
	url := flag.String("url", "http://localhost:8080/v1/assignments", "assignment API URL")
	project := flag.String("project", "pk_demo", "public project key")
	experimentCSV := flag.String("experiments", "checkout_button_v1,homepage_layout_v1", "comma-separated experiment keys")
	flag.Parse()
	if *rps < 1 || *rps > 10000 || *workers < 1 || *duration <= 0 || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "invalid options: rps must be 1..10000; workers, duration, timeout must be positive")
		os.Exit(2)
	}
	experiments := strings.Split(*experimentCSV, ",")
	for i, e := range experiments {
		experiments[i] = strings.TrimSpace(e)
		if experiments[i] == "" {
			fmt.Fprintln(os.Stderr, "invalid experiment list")
			os.Exit(2)
		}
	}

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        *workers,
		MaxIdleConnsPerHost: *workers,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 1 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: *timeout}

	result := &stats{statusCounts: map[int]int{}, errors: map[string]int{}}
	jobs := make(chan int, *workers) // bounded; queue overflow is counted, not hidden
	var wg sync.WaitGroup
	runID := time.Now().UTC().Format("20060102T150405.000000000")
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := range jobs {
				payload, _ := json.Marshal(map[string]any{
					"project_key":     *project,
					"visitor_id":      fmt.Sprintf("load-%s-%d", runID, seq),
					"experiment_keys": experiments,
				})
				start := time.Now()
				req, err := http.NewRequest(http.MethodPost, *url, bytes.NewReader(payload))
				if err != nil {
					result.add(0, time.Since(start), "invalid_request_url")
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					result.add(0, time.Since(start), "network_or_timeout")
					continue
				}
				body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
				resp.Body.Close()
				latency := time.Since(start)
				if err != nil {
					result.add(resp.StatusCode, latency, "response_read_error")
					continue
				}
				if resp.StatusCode != http.StatusOK {
					result.add(resp.StatusCode, latency, "unexpected_http_status")
					continue
				}
				var parsed assignmentResponse
				if json.Unmarshal(body, &parsed) != nil || len(parsed.Assignments) != len(experiments) {
					result.add(resp.StatusCode, latency, "invalid_json_or_count")
					continue
				}
				valid := true
				for i, a := range parsed.Assignments {
					if a.ExperimentKey != experiments[i] ||
						(a.Status != "assigned" && a.Status != "not_enrolled") ||
						(a.Status == "assigned" && a.VariantKey == "") {
						valid = false
						break
					}
				}
				if !valid {
					result.add(resp.StatusCode, latency, "invalid_assignment")
					continue
				}
				result.add(resp.StatusCode, latency, "")
			}
		}()
	}

	interval := time.Second / time.Duration(*rps)
	ticker := time.NewTicker(interval)
	timer := time.NewTimer(*duration)
	started := time.Now()
	scheduled, dropped := 0, 0
loop:
	for {
		select {
		case <-ticker.C:
			scheduled++
			select {
			case jobs <- scheduled:
			default:
				dropped++
			}
		case <-timer.C:
			break loop
		}
	}
	ticker.Stop()
	close(jobs)
	wg.Wait()
	elapsed := time.Since(started)
	sort.Float64s(result.latenciesMS)
	failures := result.completed - result.successful
	fmt.Printf("\nTarget: %d RPS for %s with %d workers\n", *rps, *duration, *workers)
	fmt.Printf("Elapsed (including drain): %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Scheduled: %d | Dropped: %d | Completed: %d | Successful: %d | Failed: %d\n", scheduled, dropped, result.completed, result.successful, failures)
	fmt.Printf("Achieved completion rate: %.1f requests/sec\n", float64(result.completed)/elapsed.Seconds())
	if result.completed > 0 {
		fmt.Printf("HTTP/request failure rate: %.2f%%\n", 100*float64(failures)/float64(result.completed))
	}
	if len(result.latenciesMS) > 0 {
		fmt.Printf("Successful-request latency: p50 %.2f ms | p95 %.2f ms | p99 %.2f ms | max %.2f ms\n",
			percentile(result.latenciesMS, .5), percentile(result.latenciesMS, .95), percentile(result.latenciesMS, .99), result.latenciesMS[len(result.latenciesMS)-1])
	}
	fmt.Printf("HTTP status counts: %v\n", result.statusCounts)
	fmt.Printf("Error categories: %v\n", result.errors)
	if dropped > 0 || failures > 0 || scheduled == 0 {
		os.Exit(1)
	}
}
