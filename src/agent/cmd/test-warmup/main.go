package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/logging"
)

func main() {
	iterations := flag.Int("n", 10, "number of iterations")
	flag.Parse()

	// Create a test HTTPS server
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond) // Simulate some processing
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Test 1: Cold connections (no warmup)
	fmt.Println("=== Test 1: Cold Connections (No Warmup) ===")
	coldLatencies := testColdConnections(server.URL, *iterations)
	coldAvg := average(coldLatencies)
	fmt.Printf("Average latency: %.2f ms\n", coldAvg)
	fmt.Printf("Min: %.2f ms, Max: %.2f ms\n", min(coldLatencies), max(coldLatencies))
	fmt.Println()

	// Test 2: With warmup
	fmt.Println("=== Test 2: With Warmup ===")
	warmLatencies := testWithWarmup(server.URL, *iterations)
	warmAvg := average(warmLatencies)
	fmt.Printf("Average latency: %.2f ms\n", warmAvg)
	fmt.Printf("Min: %.2f ms, Max: %.2f ms\n", min(warmLatencies), max(warmLatencies))
	fmt.Println()

	// Summary
	fmt.Println("=== Summary ===")
	improvement := ((coldAvg - warmAvg) / coldAvg) * 100
	fmt.Printf("Cold connection: %.2f ms\n", coldAvg)
	fmt.Printf("With warmup: %.2f ms\n", warmAvg)
	fmt.Printf("Improvement: %.2f%% (%.2f ms faster)\n", improvement, coldAvg-warmAvg)
}

func testColdConnections(url string, iterations int) []float64 {
	latencies := make([]float64, iterations)

	for i := 0; i < iterations; i++ {
		// Create fresh client for each request (cold connection)
		client := &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}

		start := time.Now()
		resp, err := client.Get(url)
		latency := time.Since(start)

		if err != nil {
			logging.Warnf("agent", "warmup", "Request %d failed: %v", i+1, err)
			latencies[i] = 0
			continue
		}
		resp.Body.Close()

		latencies[i] = float64(latency.Milliseconds())
		fmt.Printf("Request %d: %.2f ms\n", i+1, latencies[i])

		// Close connections
		client.CloseIdleConnections()

		time.Sleep(100 * time.Millisecond)
	}

	return latencies
}

func testWithWarmup(url string, iterations int) []float64 {
	latencies := make([]float64, iterations)

	// Create client with optimized transport
	transport := http.DefaultTransport.(*http.Transport).Clone()

	// Apply optimizations
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 8
	transport.IdleConnTimeout = 90 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ExpectContinueTimeout = 1 * time.Second
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	// Create warmer
	warmer := agent.NewConnectionWarmer(client, []string{url})

	for i := 0; i < iterations; i++ {
		// Warmup before request
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		warmer.WarmupAsync(ctx)
		time.Sleep(50 * time.Millisecond) // Give warmup time to complete
		cancel()

		start := time.Now()
		resp, err := client.Get(url)
		latency := time.Since(start)

		if err != nil {
			logging.Warnf("agent", "warmup", "Request %d failed: %v", i+1, err)
			latencies[i] = 0
			continue
		}
		resp.Body.Close()

		latencies[i] = float64(latency.Milliseconds())
		fmt.Printf("Request %d: %.2f ms\n", i+1, latencies[i])

		// Close connections to simulate cold start
		client.CloseIdleConnections()

		time.Sleep(100 * time.Millisecond)
	}

	return latencies
}

func average(values []float64) float64 {
	sum := 0.0
	count := 0
	for _, v := range values {
		if v > 0 {
			sum += v
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

func min(values []float64) float64 {
	minVal := values[0]
	for _, v := range values {
		if v > 0 && v < minVal {
			minVal = v
		}
	}
	return minVal
}

func max(values []float64) float64 {
	maxVal := 0.0
	for _, v := range values {
		if v > maxVal {
			maxVal = v
		}
	}
	return maxVal
}
