package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultDataset = "LocalLLaMA/typed-decisions"
	defaultConfig  = "all"
	defaultSplit   = "test"
)

type datasetRow struct {
	ID        string `json:"id"`
	Workflow  string `json:"workflow"`
	State     string `json:"state"`
	Questions string `json:"questions"`
	Gold      string `json:"gold"`
}

type rowsResponse struct {
	Rows []struct {
		Row datasetRow `json:"row"`
	} `json:"rows"`
	NumRowsTotal int `json:"num_rows_total"`
}

type apiRequest struct {
	Model     string          `json:"model"`
	State     json.RawMessage `json:"state"`
	Questions json.RawMessage `json:"questions"`
}

type apiResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Routing struct {
		Model string `json:"model"`
	} `json:"routing"`
}

type goldDecision struct {
	Label string `json:"label"`
	Type  string `json:"type"`
}

type sampleResult struct {
	Row     datasetRow
	Latency time.Duration
	Correct int
	Total   int
	Err     error
}

type config struct {
	API         string
	Dataset     string
	DatasetBase string
	DatasetCfg  string
	Split       string
	Model       string
	Limit       int
	Offset      int
	Concurrency int
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.API, "api", "http://127.0.0.1:8012", "local Laya API base URL")
	flag.StringVar(&cfg.Dataset, "dataset", defaultDataset, "Hugging Face dataset")
	flag.StringVar(&cfg.DatasetBase, "dataset-api", "https://datasets-server.huggingface.co", "Hugging Face dataset viewer API")
	flag.StringVar(&cfg.DatasetCfg, "config", defaultConfig, "dataset configuration")
	flag.StringVar(&cfg.Split, "split", defaultSplit, "dataset split")
	flag.StringVar(&cfg.Model, "model", "typed-decisions", "Laya model requested for each API call")
	flag.IntVar(&cfg.Limit, "limit", 400, "number of rows to evaluate; 0 means all available")
	flag.IntVar(&cfg.Offset, "offset", 0, "dataset row offset")
	flag.IntVar(&cfg.Concurrency, "concurrency", 1, "simultaneous API requests")
	flag.Parse()

	if err := validateConfig(cfg); err != nil {
		fatal(err)
	}

	httpClient := &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:        cfg.Concurrency,
			MaxIdleConnsPerHost: cfg.Concurrency,
		},
	}
	rows, total, err := fetchRows(httpClient, cfg)
	if err != nil {
		fatal(err)
	}
	if len(rows) == 0 {
		fatal(errors.New("dataset returned no rows"))
	}

	start := time.Now()
	results := make(chan sampleResult, len(rows))
	jobs := make(chan datasetRow)
	var workers sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for row := range jobs {
				results <- callAPI(httpClient, cfg, row)
			}
		}()
	}
	go func() {
		for _, row := range rows {
			jobs <- row
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()

	latencies := make([]time.Duration, 0, len(rows))
	correct, decisions, exactRows, errorsCount := 0, 0, 0, 0
	var firstError string
	for result := range results {
		if result.Err != nil {
			errorsCount++
			if firstError == "" {
				firstError = fmt.Sprintf("%s: %v", result.Row.ID, result.Err)
			}
			continue
		}
		latencies = append(latencies, result.Latency)
		correct += result.Correct
		decisions += result.Total
		if result.Correct == result.Total {
			exactRows++
		}
	}
	elapsed := time.Since(start)

	result := map[string]any{
		"dataset": map[string]any{
			"name":       cfg.Dataset,
			"config":     cfg.DatasetCfg,
			"split":      cfg.Split,
			"offset":     cfg.Offset,
			"rows":       len(rows),
			"rows_total": total,
		},
		"api": map[string]any{
			"endpoint":    strings.TrimRight(cfg.API, "/") + "/v1/systemone",
			"model":       cfg.Model,
			"concurrency": cfg.Concurrency,
		},
		"decisions": map[string]any{
			"total":        decisions,
			"correct":      correct,
			"accuracy":     ratio(correct, decisions),
			"exact_rows":   exactRows,
			"row_accuracy": ratio(exactRows, len(rows)-errorsCount),
		},
		"latency_ms": latencyStats(latencies),
		"elapsed_ms": float64(elapsed.Microseconds()) / 1000,
		"throughput": map[string]any{
			"requests_per_second":  float64(len(latencies)) / elapsed.Seconds(),
			"decisions_per_second": float64(decisions) / elapsed.Seconds(),
		},
		"errors": map[string]any{
			"requests": errorsCount,
			"first":    firstError,
		},
		"recorded_at": time.Now().UTC().Format(time.RFC3339),
	}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(encoded))
	if errorsCount != 0 {
		fatalf("benchmark had %d request errors: %s", errorsCount, firstError)
	}
}

func validateConfig(cfg config) error {
	if cfg.API == "" || cfg.Dataset == "" || cfg.DatasetCfg == "" || cfg.Split == "" || cfg.Model == "" {
		return errors.New("api, dataset, config, split, and model must be non-empty")
	}
	if cfg.Limit < 0 || cfg.Offset < 0 || cfg.Concurrency < 1 {
		return errors.New("limit, offset, and concurrency must be non-negative; concurrency must be positive")
	}
	return nil
}

func fetchRows(client *http.Client, cfg config) ([]datasetRow, int, error) {
	rows := make([]datasetRow, 0)
	total := 0
	remaining := cfg.Limit
	allRows := remaining == 0
	for {
		if allRows && total > 0 {
			remaining = total - cfg.Offset
		}
		if remaining <= 0 {
			break
		}
		length := remaining
		if length > 100 {
			length = 100
		}
		values := url.Values{}
		values.Set("dataset", cfg.Dataset)
		values.Set("config", cfg.DatasetCfg)
		values.Set("split", cfg.Split)
		values.Set("offset", fmt.Sprint(cfg.Offset+len(rows)))
		values.Set("length", fmt.Sprint(length))
		endpoint := strings.TrimRight(cfg.DatasetBase, "/") + "/rows?" + values.Encode()
		var response rowsResponse
		if err := getJSON(client, endpoint, &response); err != nil {
			return nil, 0, fmt.Errorf("fetch dataset rows: %w", err)
		}
		if total == 0 {
			total = response.NumRowsTotal
		}
		for _, row := range response.Rows {
			rows = append(rows, row.Row)
		}
		if len(response.Rows) == 0 || len(response.Rows) < length || len(rows) >= remaining {
			break
		}
	}
	return rows, total, nil
}

func getJSON(client *http.Client, endpoint string, dst any) error {
	response, err := client.Get(endpoint)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, bytes.TrimSpace(body))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return err
	}
	return nil
}

func callAPI(client *http.Client, cfg config, row datasetRow) sampleResult {
	result := sampleResult{Row: row}
	var state, questions json.RawMessage
	if err := json.Unmarshal([]byte(row.State), &state); err != nil {
		result.Err = fmt.Errorf("invalid state JSON: %w", err)
		return result
	}
	if err := json.Unmarshal([]byte(row.Questions), &questions); err != nil {
		result.Err = fmt.Errorf("invalid questions JSON: %w", err)
		return result
	}
	body, err := json.Marshal(apiRequest{Model: cfg.Model, State: state, Questions: questions})
	if err != nil {
		result.Err = err
		return result
	}
	request, err := http.NewRequest(http.MethodPost, strings.TrimRight(cfg.API, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		result.Err = err
		return result
	}
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	response, err := client.Do(request)
	result.Latency = time.Since(started)
	if err != nil {
		result.Err = err
		return result
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		result.Err = err
		return result
	}
	if response.StatusCode != http.StatusOK {
		result.Err = fmt.Errorf("API HTTP %d: %s", response.StatusCode, bytes.TrimSpace(responseBody))
		return result
	}
	var apiResult apiResponse
	if err := json.Unmarshal(responseBody, &apiResult); err != nil {
		result.Err = fmt.Errorf("invalid API response: %w", err)
		return result
	}
	var gold map[string]goldDecision
	if err := json.Unmarshal([]byte(row.Gold), &gold); err != nil {
		result.Err = fmt.Errorf("invalid gold JSON: %w", err)
		return result
	}
	for questionID, expected := range gold {
		answer, ok := apiResult.Answers[questionID]
		if !ok {
			result.Err = fmt.Errorf("missing answer %q", questionID)
			return result
		}
		predicted, err := predictedLabel(answer, expected.Type)
		if err != nil {
			result.Err = fmt.Errorf("question %q: %w", questionID, err)
			return result
		}
		result.Total++
		if predicted == expected.Label {
			result.Correct++
		}
	}
	return result
}

func predictedLabel(raw json.RawMessage, kind string) (string, error) {
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", err
	}
	switch kind {
	case "choice":
		var label string
		if err := json.Unmarshal(answer["choice"], &label); err != nil {
			return "", fmt.Errorf("choice is missing or invalid")
		}
		return label, nil
	case "score":
		return maxProbabilityLabel(answer["probabilities"])
	case "noul":
		var probability float64
		if err := json.Unmarshal(answer["noul"], &probability); err != nil {
			return "", fmt.Errorf("noul is missing or invalid")
		}
		if probability >= 0.5 {
			return "true", nil
		}
		return "false", nil
	default:
		return "", fmt.Errorf("unsupported gold type %q", kind)
	}
}

func maxProbabilityLabel(raw json.RawMessage) (string, error) {
	var probabilities map[string]float64
	if err := json.Unmarshal(raw, &probabilities); err != nil || len(probabilities) == 0 {
		return "", fmt.Errorf("probabilities are missing or invalid")
	}
	labels := make([]string, 0, len(probabilities))
	for label := range probabilities {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	best := labels[0]
	for _, label := range labels[1:] {
		if probabilities[label] > probabilities[best] {
			best = label
		}
	}
	return best, nil
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func latencyStats(values []time.Duration) map[string]any {
	if len(values) == 0 {
		return map[string]any{"count": 0}
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, value := range sorted {
		total += value
	}
	percentile := func(percent int) float64 {
		index := (len(sorted)*percent + 99) / 100
		if index < 1 {
			index = 1
		}
		if index > len(sorted) {
			index = len(sorted)
		}
		return float64(sorted[index-1].Microseconds()) / 1000
	}
	return map[string]any{
		"count": len(sorted),
		"min":   float64(sorted[0].Microseconds()) / 1000,
		"mean":  float64(total.Microseconds()) / float64(len(sorted)) / 1000,
		"p50":   percentile(50),
		"p95":   percentile(95),
		"p99":   percentile(99),
		"max":   float64(sorted[len(sorted)-1].Microseconds()) / 1000,
	}
}

func fatal(err error) {
	fatalf("%v", err)
}

func fatalf(format string, args ...any) {
	fmt.Printf("benchmark_error: "+format+"\n", args...)
	os.Exit(1)
}
