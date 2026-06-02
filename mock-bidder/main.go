package main

import (
  "encoding/json"
  "fmt"
  "log"
  "math/rand"
  "net/http"
  "os"
  "strconv"
  "strings"
  "sync"
  "time"

  "github.com/prometheus/client_golang/prometheus"
  "github.com/prometheus/client_golang/prometheus/promhttp"
)

type bannerFormat struct {
  W int `json:"w"`
  H int `json:"h"`
}

type banner struct {
  W      int            `json:"w"`
  H      int            `json:"h"`
  Format []bannerFormat `json:"format"`
}

type imp struct {
  ID     string  `json:"id"`
  Banner *banner `json:"banner"`
}

type bidRequest struct {
  ID  string `json:"id"`
  Imp []imp  `json:"imp"`
}

type bid struct {
  ID    string  `json:"id"`
  ImpID string  `json:"impid"`
  Price float64 `json:"price"`
  Adm   string  `json:"adm"`
  CrID  string  `json:"crid"`
  W     int     `json:"w"`
  H     int     `json:"h"`
}

type seatBid struct {
  Bid  []bid  `json:"bid"`
  Seat string `json:"seat,omitempty"`
}

type bidResponse struct {
  ID      string    `json:"id"`
  SeatBid []seatBid `json:"seatbid"`
  Cur     string    `json:"cur"`
}

type serverConfig struct {
  Port       string
  CPM        float64
  NoBidRate  float64
  ErrorRate  float64
  Latency    time.Duration
  CreativeID string
}

var (
  rngMu sync.Mutex
  rng   = rand.New(rand.NewSource(1))

  requestsTotal = prometheus.NewCounterVec(
    prometheus.CounterOpts{
      Name: "mock_bidder_requests_total",
      Help: "Count of incoming bid requests by status.",
    },
    []string{"status"},
  )

  responsesTotal = prometheus.NewCounterVec(
    prometheus.CounterOpts{
      Name: "mock_bidder_bid_responses_total",
      Help: "Count of bid responses by result.",
    },
    []string{"result"},
  )

  requestDuration = prometheus.NewHistogramVec(
    prometheus.HistogramOpts{
      Name:    "mock_bidder_request_duration_seconds",
      Help:    "Bid request latency in seconds.",
      Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.35, 0.5, 0.75, 1, 2},
    },
    []string{"status"},
  )
)

func main() {
  prometheus.MustRegister(requestsTotal, responsesTotal, requestDuration)

  cfg := loadConfig()

  mux := http.NewServeMux()
  mux.HandleFunc("/healthz", healthz)
  mux.Handle("/metrics", promhttp.Handler())
  mux.HandleFunc("/currency", currency)
  mux.HandleFunc("/openrtb2", func(w http.ResponseWriter, r *http.Request) {
    handleBid(cfg, w, r)
  })
  mux.HandleFunc("/bid", func(w http.ResponseWriter, r *http.Request) {
    handleBid(cfg, w, r)
  })

  server := &http.Server{
    Addr:              ":" + cfg.Port,
    Handler:           logging(mux),
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       10 * time.Second,
    WriteTimeout:      10 * time.Second,
    IdleTimeout:       30 * time.Second,
  }

  log.Printf("mock-bidder listening on :%s", cfg.Port)
  if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
    log.Fatalf("server error: %v", err)
  }
}

func loadConfig() serverConfig {
  return serverConfig{
    Port:       getEnv("BIDDER_PORT", "8081"),
    CPM:        getEnvFloat("BIDDER_CPM", 1.23),
    NoBidRate:  clampRate(getEnvFloat("BIDDER_NOBID_RATE", 0)),
    ErrorRate:  clampRate(getEnvFloat("BIDDER_ERROR_RATE", 0)),
    Latency:    time.Duration(getEnvInt("BIDDER_LATENCY_MS", 0)) * time.Millisecond,
    CreativeID: getEnv("BIDDER_CREATIVE_ID", "mock-creative-1"),
  }
}

func handleBid(baseCfg serverConfig, w http.ResponseWriter, r *http.Request) {
  start := time.Now()
  cfg := applyOverrides(baseCfg, r)

  if r.Method != http.MethodPost {
    respondStatus(w, http.StatusMethodNotAllowed, "error", "error", start)
    return
  }

  if cfg.Latency > 0 {
    time.Sleep(cfg.Latency)
  }

  var req bidRequest
  if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
    respondStatus(w, http.StatusBadRequest, "bad_request", "error", start)
    return
  }

  impIDs := make([]string, 0, len(req.Imp))
  for _, imp := range req.Imp {
    if imp.ID != "" {
      impIDs = append(impIDs, imp.ID)
    }
  }

  roll := randFloat()
  if roll < cfg.ErrorRate {
    log.Printf("request_id=%s imp_ids=%s status=error", req.ID, strings.Join(impIDs, ","))
    respondStatus(w, http.StatusInternalServerError, "error", "error", start)
    return
  }

  if roll < cfg.ErrorRate+cfg.NoBidRate {
    log.Printf("request_id=%s imp_ids=%s status=no_bid", req.ID, strings.Join(impIDs, ","))
    respondStatus(w, http.StatusNoContent, "nobid", "nobid", start)
    return
  }

  bids := make([]bid, 0, len(req.Imp))
  for i, imp := range req.Imp {
    impID := imp.ID
    if impID == "" {
      impID = fmt.Sprintf("imp-%d", i+1)
    }
    w, h := resolveSize(imp)
    bids = append(bids, bid{
      ID:    fmt.Sprintf("bid-%s-%s", req.ID, impID),
      ImpID: impID,
      Price: cfg.CPM,
      Adm:   creativeHTML(w, h, cfg.CPM),
      CrID:  cfg.CreativeID,
      W:     w,
      H:     h,
    })
  }

  if len(bids) == 0 {
    log.Printf("request_id=%s imp_ids=%s status=no_bid", req.ID, strings.Join(impIDs, ","))
    respondStatus(w, http.StatusNoContent, "nobid", "nobid", start)
    return
  }

  response := bidResponse{
    ID: req.ID,
    SeatBid: []seatBid{
      {
        Bid:  bids,
        Seat: "mock-bidder",
      },
    },
    Cur: "USD",
  }

  payload, err := json.Marshal(response)
  if err != nil {
    log.Printf("request_id=%s imp_ids=%s status=error", req.ID, strings.Join(impIDs, ","))
    respondStatus(w, http.StatusInternalServerError, "error", "error", start)
    return
  }

  w.Header().Set("Content-Type", "application/json")
  w.WriteHeader(http.StatusOK)
  _, _ = w.Write(payload)

  log.Printf("request_id=%s imp_ids=%s status=bid", req.ID, strings.Join(impIDs, ","))
  observeMetrics("ok", "bid", start)
}

func respondStatus(w http.ResponseWriter, status int, metricStatus string, result string, start time.Time) {
  w.WriteHeader(status)
  observeMetrics(metricStatus, result, start)
}

func observeMetrics(status string, result string, start time.Time) {
  requestsTotal.WithLabelValues(status).Inc()
  responsesTotal.WithLabelValues(result).Inc()
  requestDuration.WithLabelValues(status).Observe(time.Since(start).Seconds())
}

func resolveSize(imp imp) (int, int) {
  if imp.Banner != nil {
    if imp.Banner.W > 0 && imp.Banner.H > 0 {
      return imp.Banner.W, imp.Banner.H
    }
    if len(imp.Banner.Format) > 0 {
      if imp.Banner.Format[0].W > 0 && imp.Banner.Format[0].H > 0 {
        return imp.Banner.Format[0].W, imp.Banner.Format[0].H
      }
    }
  }
  return 300, 250
}

func creativeHTML(w int, h int, cpm float64) string {
  return fmt.Sprintf(
    "<div style='width:%dpx;height:%dpx;background:#f8f8f8;border:1px solid #999;display:flex;align-items:center;justify-content:center;font-family:Arial,sans-serif;font-size:14px;color:#333;'>Mock Bidder - $%.2f</div>",
    w,
    h,
    cpm,
  )
}

func currency(w http.ResponseWriter, _ *http.Request) {
  w.Header().Set("Content-Type", "application/json")
  _, _ = w.Write([]byte(`{"conversions":{"USD":{"USD":1}}}`))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
  w.Header().Set("Content-Type", "text/plain")
  _, _ = w.Write([]byte("ok"))
}

func logging(next http.Handler) http.Handler {
  return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    next.ServeHTTP(w, r)
    log.Printf("method=%s path=%s duration_ms=%d", r.Method, r.URL.Path, time.Since(start).Milliseconds())
  })
}

func applyOverrides(cfg serverConfig, r *http.Request) serverConfig {
  q := r.URL.Query()
  if value := q.Get("nobid_rate"); value != "" {
    if parsed, err := strconv.ParseFloat(value, 64); err == nil {
      cfg.NoBidRate = clampRate(parsed)
    }
  }
  if value := q.Get("error_rate"); value != "" {
    if parsed, err := strconv.ParseFloat(value, 64); err == nil {
      cfg.ErrorRate = clampRate(parsed)
    }
  }
  if value := q.Get("latency_ms"); value != "" {
    if parsed, err := strconv.Atoi(value); err == nil {
      cfg.Latency = time.Duration(parsed) * time.Millisecond
    }
  }
  if value := q.Get("cpm"); value != "" {
    if parsed, err := strconv.ParseFloat(value, 64); err == nil {
      cfg.CPM = parsed
    }
  }
  return cfg
}

func randFloat() float64 {
  rngMu.Lock()
  defer rngMu.Unlock()
  return rng.Float64()
}

func clampRate(rate float64) float64 {
  if rate < 0 {
    return 0
  }
  if rate > 1 {
    return 1
  }
  return rate
}

func getEnv(key, fallback string) string {
  if value := os.Getenv(key); value != "" {
    return value
  }
  return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
  if value := os.Getenv(key); value != "" {
    if parsed, err := strconv.ParseFloat(value, 64); err == nil {
      return parsed
    }
  }
  return fallback
}

func getEnvInt(key string, fallback int) int {
  if value := os.Getenv(key); value != "" {
    if parsed, err := strconv.Atoi(value); err == nil {
      return parsed
    }
  }
  return fallback
}
