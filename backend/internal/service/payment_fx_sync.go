package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
)

const (
	SettingPaymentFXSyncEnabled       = "PAYMENT_FX_SYNC_ENABLED"
	SettingPaymentFXSyncURLs          = "PAYMENT_FX_SYNC_URLS"
	SettingPaymentFXSyncIntervalHours = "PAYMENT_FX_SYNC_INTERVAL_HOURS"
	SettingPaymentFXSyncMarkupPercent = "PAYMENT_FX_SYNC_MARKUP_PERCENT"

	SettingPaymentFXSyncUpdateSubscriptionRate = "PAYMENT_FX_SYNC_UPDATE_SUBSCRIPTION_RATE"
	SettingPaymentFXSyncModelPrices            = "PAYMENT_FX_SYNC_MODEL_PRICES"

	SettingPaymentFXSyncLastAttemptAt        = "PAYMENT_FX_SYNC_LAST_ATTEMPT_AT"
	SettingPaymentFXSyncLastSuccessAt        = "PAYMENT_FX_SYNC_LAST_SUCCESS_AT"
	SettingPaymentFXSyncLastRate             = "PAYMENT_FX_SYNC_LAST_RATE"
	SettingPaymentFXSyncLastMultiplier       = "PAYMENT_FX_SYNC_LAST_MULTIPLIER"
	SettingPaymentFXSyncLastSubscriptionRate = "PAYMENT_FX_SYNC_LAST_SUBSCRIPTION_RATE"
	SettingPaymentFXSyncLastSource           = "PAYMENT_FX_SYNC_LAST_SOURCE"
	SettingPaymentFXSyncLastError            = "PAYMENT_FX_SYNC_LAST_ERROR"
)

const (
	defaultPaymentFXSyncIntervalHours = 24
	minPaymentFXSyncIntervalHours     = 1
	maxPaymentFXSyncIntervalHours     = 168
	maxPaymentFXSyncURLs              = 10
	maxPaymentFXSyncModelPrices       = 100
	maxPaymentFXSyncModelPriceGroups  = 100
	maxPaymentFXSyncCNYPrice          = 1e9

	paymentFXSyncCheckInterval = time.Minute
	paymentFXSyncInitialDelay  = 30 * time.Second
	paymentFXSyncTimeout       = 45 * time.Second
	paymentFXSyncRetryDelay    = time.Hour

	paymentFXSyncLeaderLockKey = "payment:fx:sync:leader"
	paymentFXSyncLeaderLockTTL = 10 * time.Minute

	paymentFXSyncMinRate         = 1.0
	paymentFXSyncMaxRate         = 20.0
	paymentFXSyncMaxResponseSize = 1 << 20
	paymentFXSyncUserAgent       = "sub2api-fx-sync/1.0"
)

var (
	ErrPaymentFXSyncUnavailable = infraerrors.InternalServer("PAYMENT_FX_SYNC_UNAVAILABLE", "payment FX sync is unavailable")
	ErrPaymentFXSyncInProgress  = infraerrors.Conflict("PAYMENT_FX_SYNC_IN_PROGRESS", "an FX sync is already running")
)

// PaymentFXModelPrice pins a model's official domestic CNY price (per 1M tokens)
// so every sync recomputes the USD group pricing cards at the live FX rate.
// CNYCacheRead == 0 leaves the cache-read price untouched.
type PaymentFXModelPrice struct {
	Model        string  `json:"model"`
	CNYInput     float64 `json:"cny_input"`
	CNYOutput    float64 `json:"cny_output"`
	CNYCacheRead float64 `json:"cny_cache_read,omitempty"`
	GroupIDs     []int64 `json:"group_ids"`
}

// PaymentFXSyncConfig is both the admin configuration and the latest sync state.
type PaymentFXSyncConfig struct {
	Enabled                bool                  `json:"enabled"`
	URLs                   []string              `json:"urls"`
	IntervalHours          int                   `json:"interval_hours"`
	MarkupPercent          float64               `json:"markup_percent"`
	UpdateSubscriptionRate bool                  `json:"update_subscription_rate"`
	ModelPrices            []PaymentFXModelPrice `json:"model_prices"`

	LastAttemptAt        string  `json:"last_attempt_at,omitempty"`
	LastSuccessAt        string  `json:"last_success_at,omitempty"`
	LastRate             float64 `json:"last_rate,omitempty"`
	LastMultiplier       float64 `json:"last_multiplier,omitempty"`
	LastSubscriptionRate float64 `json:"last_subscription_rate,omitempty"`
	LastSource           string  `json:"last_source,omitempty"`
	LastError            string  `json:"last_error,omitempty"`
	NextRunAt            string  `json:"next_run_at,omitempty"`
}

// UpdatePaymentFXSyncRequest replaces the full FX sync configuration.
type UpdatePaymentFXSyncRequest struct {
	Enabled                bool                  `json:"enabled"`
	URLs                   []string              `json:"urls"`
	IntervalHours          int                   `json:"interval_hours"`
	MarkupPercent          float64               `json:"markup_percent"`
	UpdateSubscriptionRate bool                  `json:"update_subscription_rate"`
	ModelPrices            []PaymentFXModelPrice `json:"model_prices"`
}

// PaymentFXSyncService periodically fetches a USD/CNY FX rate and updates the
// balance recharge multiplier. It is opt-in and stores config/status in the
// existing settings KV, so no migration is required.
type PaymentFXSyncService struct {
	settingRepo      SettingRepository
	paymentConfigSvc *PaymentConfigService
	adminService     AdminService
	leaderLockCache  LeaderLockCache
	db               *sql.DB

	httpClient *http.Client
	now        func() time.Time
	instanceID string

	checkInterval time.Duration
	initialDelay  time.Duration

	stopCh    chan struct{}
	stopOnce  sync.Once
	startOnce sync.Once
	wg        sync.WaitGroup
}

func NewPaymentFXSyncService(
	settingRepo SettingRepository,
	paymentConfigSvc *PaymentConfigService,
	adminService AdminService,
	leaderLockCache LeaderLockCache,
	db *sql.DB,
) *PaymentFXSyncService {
	return &PaymentFXSyncService{
		settingRepo:      settingRepo,
		paymentConfigSvc: paymentConfigSvc,
		adminService:     adminService,
		leaderLockCache:  leaderLockCache,
		db:               db,
		httpClient:       newSSRFSafeHTTPClient(15 * time.Second),
		now:              time.Now,
		instanceID:       uuid.NewString(),
		checkInterval:    paymentFXSyncCheckInterval,
		initialDelay:     paymentFXSyncInitialDelay,
		stopCh:           make(chan struct{}),
	}
}

// Start launches the periodic due check. It is safe to call once.
func (s *PaymentFXSyncService) Start() {
	if s == nil || s.settingRepo == nil || s.paymentConfigSvc == nil {
		return
	}
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			delay := s.initialDelay
			if delay <= 0 {
				delay = time.Millisecond
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
				s.runScheduled()
			case <-s.stopCh:
				timer.Stop()
				return
			}

			ticker := time.NewTicker(s.checkInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					s.runScheduled()
				case <-s.stopCh:
					return
				}
			}
		}()
	})
}

func (s *PaymentFXSyncService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

// GetConfig returns the current configuration and last sync state.
func (s *PaymentFXSyncService) GetConfig(ctx context.Context) (*PaymentFXSyncConfig, error) {
	if s == nil || s.settingRepo == nil {
		return nil, ErrPaymentFXSyncUnavailable
	}
	vals, err := s.settingRepo.GetMultiple(ctx, paymentFXSyncKeys())
	if err != nil {
		return nil, fmt.Errorf("get payment FX sync settings: %w", err)
	}
	cfg := parsePaymentFXSyncConfig(vals)
	cfg.NextRunAt = paymentFXNextRunAt(cfg, s.now().UTC())
	return cfg, nil
}

// UpdateConfig validates and persists the full FX sync configuration.
func (s *PaymentFXSyncService) UpdateConfig(ctx context.Context, req UpdatePaymentFXSyncRequest) (*PaymentFXSyncConfig, error) {
	if s == nil || s.settingRepo == nil {
		return nil, ErrPaymentFXSyncUnavailable
	}

	urls, err := normalizePaymentFXSyncURLs(req.URLs)
	if err != nil {
		return nil, err
	}
	if req.Enabled && len(urls) == 0 {
		return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_URL_REQUIRED", "at least one FX API URL is required when sync is enabled")
	}
	interval := req.IntervalHours
	if interval < minPaymentFXSyncIntervalHours || interval > maxPaymentFXSyncIntervalHours {
		return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_INVALID_INTERVAL", fmt.Sprintf("sync interval must be between %d and %d hours", minPaymentFXSyncIntervalHours, maxPaymentFXSyncIntervalHours))
	}
	markup := req.MarkupPercent
	if math.IsNaN(markup) || math.IsInf(markup, 0) || markup < 0 || markup > 100 {
		return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_INVALID_MARKUP", "markup percent must be between 0 and 100")
	}
	markup = math.Round(markup*100) / 100

	modelPrices, err := normalizePaymentFXModelPrices(req.ModelPrices)
	if err != nil {
		return nil, err
	}
	modelPricesJSON, err := json.Marshal(modelPrices)
	if err != nil {
		return nil, fmt.Errorf("encode model prices: %w", err)
	}

	values := map[string]string{
		SettingPaymentFXSyncEnabled:                formatBoolString(req.Enabled),
		SettingPaymentFXSyncURLs:                   strings.Join(urls, "\n"),
		SettingPaymentFXSyncIntervalHours:          strconv.Itoa(interval),
		SettingPaymentFXSyncMarkupPercent:          strconv.FormatFloat(markup, 'f', -1, 64),
		SettingPaymentFXSyncUpdateSubscriptionRate: formatBoolString(req.UpdateSubscriptionRate),
		SettingPaymentFXSyncModelPrices:            string(modelPricesJSON),
	}
	if err := s.settingRepo.SetMultiple(ctx, values); err != nil {
		return nil, fmt.Errorf("save payment FX sync settings: %w", err)
	}
	return s.GetConfig(ctx)
}

// SyncNow runs one sync immediately, even when the periodic sync is disabled.
func (s *PaymentFXSyncService) SyncNow(ctx context.Context) (*PaymentFXSyncConfig, error) {
	if s == nil || s.settingRepo == nil || s.paymentConfigSvc == nil {
		return nil, ErrPaymentFXSyncUnavailable
	}
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	if len(cfg.URLs) == 0 {
		return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_URL_REQUIRED", "at least one FX API URL is required")
	}

	syncCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		syncCtx, cancel = context.WithTimeout(ctx, paymentFXSyncTimeout)
		defer cancel()
	}

	release, ok := tryAcquireSingletonLeaderLock(syncCtx, s.leaderLockCache, s.db, paymentFXSyncLeaderLockKey, s.instanceID, paymentFXSyncLeaderLockTTL)
	if !ok {
		return cfg, ErrPaymentFXSyncInProgress
	}
	defer release()

	if err := s.syncOnce(syncCtx, cfg); err != nil {
		state, stateErr := s.GetConfig(ctx)
		if stateErr != nil {
			return nil, err
		}
		return state, err
	}
	return s.GetConfig(ctx)
}

func (s *PaymentFXSyncService) runScheduled() {
	ctx, cancel := context.WithTimeout(context.Background(), paymentFXSyncTimeout)
	defer cancel()

	cfg, err := s.GetConfig(ctx)
	if err != nil {
		logger.LegacyPrintf("service.payment_fx_sync", "[PaymentFXSync] load settings failed: %v", err)
		return
	}
	if !cfg.Enabled || !paymentFXSyncDue(cfg, s.now().UTC()) {
		return
	}

	release, ok := tryAcquireSingletonLeaderLock(ctx, s.leaderLockCache, s.db, paymentFXSyncLeaderLockKey, s.instanceID, paymentFXSyncLeaderLockTTL)
	if !ok {
		return
	}
	defer release()

	if err := s.syncOnce(ctx, cfg); err != nil {
		logger.LegacyPrintf("service.payment_fx_sync", "[PaymentFXSync] sync failed: %v", err)
		return
	}
	logger.LegacyPrintf("service.payment_fx_sync", "[PaymentFXSync] synced rate=%.6f multiplier=%.6f source=%s", cfg.LastRate, cfg.LastMultiplier, cfg.LastSource)
}

func (s *PaymentFXSyncService) syncOnce(ctx context.Context, cfg *PaymentFXSyncConfig) error {
	startedAt := s.now().UTC()
	rate, source, err := s.fetchRate(ctx, cfg.URLs)
	if err != nil {
		s.recordFailure(ctx, startedAt, err)
		return err
	}
	if rate < paymentFXSyncMinRate || rate > paymentFXSyncMaxRate {
		err := fmt.Errorf("fetched CNY/USD rate %.6f is outside the allowed range [%.1f, %.1f]", rate, paymentFXSyncMinRate, paymentFXSyncMaxRate)
		s.recordFailure(ctx, startedAt, err)
		return err
	}

	effectiveRate := rate * (1 + cfg.MarkupPercent/100)
	if effectiveRate <= 0 {
		err := fmt.Errorf("effective CNY/USD rate is invalid: %.6f", effectiveRate)
		s.recordFailure(ctx, startedAt, err)
		return err
	}
	multiplier := math.Round((1/effectiveRate)*1e6) / 1e6
	subscriptionRate := math.Round(effectiveRate*1e6) / 1e6

	update := UpdatePaymentConfigRequest{BalanceRechargeMultiplier: &multiplier}
	if cfg.UpdateSubscriptionRate {
		update.SubscriptionUSDToCNYRate = &subscriptionRate
	}
	if err := s.paymentConfigSvc.UpdatePaymentConfig(ctx, update); err != nil {
		s.recordFailure(ctx, startedAt, err)
		return err
	}

	if len(cfg.ModelPrices) > 0 {
		if err := s.applyModelPrices(ctx, cfg.ModelPrices, effectiveRate); err != nil {
			s.recordFailure(ctx, startedAt, err)
			return err
		}
	}

	status := map[string]string{
		SettingPaymentFXSyncLastAttemptAt:        startedAt.Format(time.RFC3339),
		SettingPaymentFXSyncLastSuccessAt:        startedAt.Format(time.RFC3339),
		SettingPaymentFXSyncLastRate:             strconv.FormatFloat(math.Round(rate*1e6)/1e6, 'f', -1, 64),
		SettingPaymentFXSyncLastMultiplier:       strconv.FormatFloat(multiplier, 'f', -1, 64),
		SettingPaymentFXSyncLastSubscriptionRate: "",
		SettingPaymentFXSyncLastSource:           source,
		SettingPaymentFXSyncLastError:            "",
	}
	if cfg.UpdateSubscriptionRate {
		status[SettingPaymentFXSyncLastSubscriptionRate] = strconv.FormatFloat(subscriptionRate, 'f', -1, 64)
	}
	if err := s.settingRepo.SetMultiple(ctx, status); err != nil {
		logger.LegacyPrintf("service.payment_fx_sync", "[PaymentFXSync] save success status failed: %v", err)
	}

	cfg.LastAttemptAt = status[SettingPaymentFXSyncLastAttemptAt]
	cfg.LastSuccessAt = status[SettingPaymentFXSyncLastSuccessAt]
	cfg.LastRate = rate
	cfg.LastMultiplier = multiplier
	cfg.LastSource = source
	cfg.LastError = ""
	cfg.LastSubscriptionRate = 0
	if cfg.UpdateSubscriptionRate {
		cfg.LastSubscriptionRate = subscriptionRate
	}
	return nil
}

func (s *PaymentFXSyncService) recordFailure(ctx context.Context, at time.Time, syncErr error) {
	msg := syncErr.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	if err := s.settingRepo.SetMultiple(ctx, map[string]string{
		SettingPaymentFXSyncLastAttemptAt: at.Format(time.RFC3339),
		SettingPaymentFXSyncLastError:     msg,
	}); err != nil {
		logger.LegacyPrintf("service.payment_fx_sync", "[PaymentFXSync] save failure status failed: %v", err)
	}
}

func (s *PaymentFXSyncService) fetchRate(ctx context.Context, urls []string) (float64, string, error) {
	if len(urls) == 0 {
		return 0, "", fmt.Errorf("no FX API URLs configured")
	}
	errs := make([]string, 0, len(urls))
	for _, raw := range urls {
		rate, err := s.fetchRateFromURL(ctx, raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", raw, err))
			continue
		}
		return rate, raw, nil
	}
	return 0, "", fmt.Errorf("all FX API sources failed: %s", strings.Join(errs, "; "))
}

func (s *PaymentFXSyncService) fetchRateFromURL(ctx context.Context, rawURL string) (float64, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return 0, fmt.Errorf("invalid FX API URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil) //nolint:gosec // admin-configured FX URL; the SSRF-safe dialer only permits public hosts
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", paymentFXSyncUserAgent)

	resp, err := s.httpClient.Do(req) //nolint:gosec // request URL is admin-configured and validated by the SSRF-safe dialer
	if err != nil {
		return 0, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Base            string             `json:"base"`
		BaseCode        string             `json:"base_code"`
		Rates           map[string]float64 `json:"rates"`
		ConversionRates map[string]float64 `json:"conversion_rates"`
	}
	limited := io.LimitReader(resp.Body, paymentFXSyncMaxResponseSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}
	if len(body) > paymentFXSyncMaxResponseSize {
		return 0, fmt.Errorf("response exceeds %d bytes", paymentFXSyncMaxResponseSize)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("decode JSON: %w", err)
	}
	rates := payload.Rates
	if len(rates) == 0 {
		rates = payload.ConversionRates
	}
	if len(rates) == 0 {
		return 0, fmt.Errorf("response has no rates map")
	}

	cny, ok := rates["CNY"]
	if !ok || cny <= 0 {
		return 0, fmt.Errorf("response has no valid CNY rate")
	}
	base := strings.TrimSpace(payload.BaseCode)
	if base == "" {
		base = strings.TrimSpace(payload.Base)
	}
	rate := cny
	if usd, ok := rates["USD"]; ok && usd > 0 {
		rate = cny / usd
	} else if !strings.EqualFold(base, "USD") {
		return 0, fmt.Errorf("response base is %q and has no USD rate", base)
	}
	if rate < paymentFXSyncMinRate || rate > paymentFXSyncMaxRate {
		return 0, fmt.Errorf("CNY/USD rate %.6f is outside the allowed range [%.1f, %.1f]", rate, paymentFXSyncMinRate, paymentFXSyncMaxRate)
	}
	return rate, nil
}

func parsePaymentFXSyncConfig(vals map[string]string) *PaymentFXSyncConfig {
	cfg := &PaymentFXSyncConfig{
		Enabled:                vals[SettingPaymentFXSyncEnabled] == "true",
		URLs:                   splitPaymentFXSyncURLs(vals[SettingPaymentFXSyncURLs]),
		IntervalHours:          pcParseInt(vals[SettingPaymentFXSyncIntervalHours], defaultPaymentFXSyncIntervalHours),
		MarkupPercent:          pcParseFloat(vals[SettingPaymentFXSyncMarkupPercent], 0),
		UpdateSubscriptionRate: vals[SettingPaymentFXSyncUpdateSubscriptionRate] == "true",
		ModelPrices:            parsePaymentFXModelPrices(vals[SettingPaymentFXSyncModelPrices]),
		LastAttemptAt:          strings.TrimSpace(vals[SettingPaymentFXSyncLastAttemptAt]),
		LastSuccessAt:          strings.TrimSpace(vals[SettingPaymentFXSyncLastSuccessAt]),
		LastRate:               pcParseFloat(vals[SettingPaymentFXSyncLastRate], 0),
		LastMultiplier:         pcParseFloat(vals[SettingPaymentFXSyncLastMultiplier], 0),
		LastSubscriptionRate:   pcParseFloat(vals[SettingPaymentFXSyncLastSubscriptionRate], 0),
		LastSource:             strings.TrimSpace(vals[SettingPaymentFXSyncLastSource]),
		LastError:              strings.TrimSpace(vals[SettingPaymentFXSyncLastError]),
	}
	if cfg.IntervalHours < minPaymentFXSyncIntervalHours || cfg.IntervalHours > maxPaymentFXSyncIntervalHours {
		cfg.IntervalHours = defaultPaymentFXSyncIntervalHours
	}
	if math.IsNaN(cfg.MarkupPercent) || math.IsInf(cfg.MarkupPercent, 0) || cfg.MarkupPercent < 0 || cfg.MarkupPercent > 100 {
		cfg.MarkupPercent = 0
	}
	return cfg
}

func paymentFXSyncKeys() []string {
	return []string{
		SettingPaymentFXSyncEnabled,
		SettingPaymentFXSyncURLs,
		SettingPaymentFXSyncIntervalHours,
		SettingPaymentFXSyncMarkupPercent,
		SettingPaymentFXSyncUpdateSubscriptionRate,
		SettingPaymentFXSyncModelPrices,
		SettingPaymentFXSyncLastAttemptAt,
		SettingPaymentFXSyncLastSuccessAt,
		SettingPaymentFXSyncLastRate,
		SettingPaymentFXSyncLastMultiplier,
		SettingPaymentFXSyncLastSubscriptionRate,
		SettingPaymentFXSyncLastSource,
		SettingPaymentFXSyncLastError,
	}
}

func splitPaymentFXSyncURLs(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';'
	})
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

func normalizePaymentFXSyncURLs(raw []string) ([]string, error) {
	urls := splitPaymentFXSyncURLs(strings.Join(raw, "\n"))
	if len(urls) > maxPaymentFXSyncURLs {
		return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_TOO_MANY_URLS", fmt.Sprintf("at most %d FX API URLs are allowed", maxPaymentFXSyncURLs))
	}
	for _, rawURL := range urls {
		parsed, err := url.Parse(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_INVALID_URL", fmt.Sprintf("invalid FX API URL: %s", rawURL))
		}
	}
	return urls, nil
}

func normalizePaymentFXModelPrices(entries []PaymentFXModelPrice) ([]PaymentFXModelPrice, error) {
	if len(entries) > maxPaymentFXSyncModelPrices {
		return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_TOO_MANY_MODEL_PRICES", fmt.Sprintf("at most %d model price entries are allowed", maxPaymentFXSyncModelPrices))
	}
	out := make([]PaymentFXModelPrice, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		model := strings.TrimSpace(entry.Model)
		if model == "" {
			return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_MODEL_REQUIRED", "model price entry requires a model name")
		}
		if _, ok := seen[model]; ok {
			return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_MODEL_DUPLICATE", fmt.Sprintf("duplicate model price entry: %s", model))
		}
		seen[model] = struct{}{}
		if !validPaymentFXCNYPrice(entry.CNYInput) || !validPaymentFXCNYPrice(entry.CNYOutput) {
			return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_INVALID_MODEL_PRICE", fmt.Sprintf("model %s: cny_input and cny_output must be positive finite numbers", model))
		}
		if entry.CNYCacheRead != 0 && !validPaymentFXCNYPrice(entry.CNYCacheRead) {
			return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_INVALID_MODEL_PRICE", fmt.Sprintf("model %s: cny_cache_read must be a positive finite number or 0", model))
		}
		groupIDs := make([]int64, 0, len(entry.GroupIDs))
		seenGroups := make(map[int64]struct{}, len(entry.GroupIDs))
		for _, id := range entry.GroupIDs {
			if id <= 0 {
				return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_INVALID_MODEL_GROUP", fmt.Sprintf("model %s: group IDs must be positive", model))
			}
			if _, ok := seenGroups[id]; ok {
				continue
			}
			seenGroups[id] = struct{}{}
			groupIDs = append(groupIDs, id)
		}
		if len(groupIDs) == 0 {
			return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_MODEL_GROUP_REQUIRED", fmt.Sprintf("model %s: at least one group is required", model))
		}
		if len(groupIDs) > maxPaymentFXSyncModelPriceGroups {
			return nil, infraerrors.BadRequest("PAYMENT_FX_SYNC_TOO_MANY_MODEL_GROUPS", fmt.Sprintf("model %s: at most %d groups are allowed", model, maxPaymentFXSyncModelPriceGroups))
		}
		out = append(out, PaymentFXModelPrice{
			Model:        model,
			CNYInput:     entry.CNYInput,
			CNYOutput:    entry.CNYOutput,
			CNYCacheRead: entry.CNYCacheRead,
			GroupIDs:     groupIDs,
		})
	}
	return out, nil
}

func validPaymentFXCNYPrice(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) && v <= maxPaymentFXSyncCNYPrice
}

func parsePaymentFXModelPrices(raw string) []PaymentFXModelPrice {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var entries []PaymentFXModelPrice
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	normalized, err := normalizePaymentFXModelPrices(entries)
	if err != nil {
		return nil
	}
	return normalized
}

// paymentFXCNYToTokenPrice converts an official CNY price per 1M tokens into a
// USD per-token price: round the per-1M USD amount to 6 decimals first, then
// scale down, so the displayed official price keeps 6-decimal precision.
func paymentFXCNYToTokenPrice(cnyPerMillion, effectiveRate float64) float64 {
	usdPerMillion := math.Round(cnyPerMillion/effectiveRate*1e6) / 1e6
	return usdPerMillion / 1e6
}

// applyModelPrices upserts the FX-linked pricing cards on every referenced
// group. Groups touched by several entries are written once.
func (s *PaymentFXSyncService) applyModelPrices(ctx context.Context, entries []PaymentFXModelPrice, effectiveRate float64) error {
	if s.adminService == nil {
		return fmt.Errorf("model price sync requires the group admin service")
	}
	type groupUpdate struct {
		pricing []ChannelModelPricing
	}
	updates := make(map[int64]*groupUpdate)
	errs := make([]string, 0)
	for _, entry := range entries {
		input := paymentFXCNYToTokenPrice(entry.CNYInput, effectiveRate)
		output := paymentFXCNYToTokenPrice(entry.CNYOutput, effectiveRate)
		var cacheRead *float64
		if entry.CNYCacheRead > 0 {
			v := paymentFXCNYToTokenPrice(entry.CNYCacheRead, effectiveRate)
			cacheRead = &v
		}
		for _, groupID := range entry.GroupIDs {
			update, ok := updates[groupID]
			if !ok {
				group, err := s.adminService.GetGroup(ctx, groupID)
				if err != nil {
					errs = append(errs, fmt.Sprintf("group %d: %v", groupID, err))
					continue
				}
				pricing := make([]ChannelModelPricing, len(group.ModelPricing))
				for i := range group.ModelPricing {
					pricing[i] = group.ModelPricing[i].Clone()
				}
				update = &groupUpdate{pricing: pricing}
				updates[groupID] = update
			}
			update.pricing = upsertPaymentFXModelCard(update.pricing, entry.Model, input, output, cacheRead)
		}
	}
	for groupID, update := range updates {
		if _, err := s.adminService.UpdateGroup(ctx, groupID, &UpdateGroupInput{ModelPricing: &update.pricing}); err != nil {
			errs = append(errs, fmt.Sprintf("group %d: %v", groupID, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("update model prices: %s", strings.Join(errs, "; "))
	}
	logger.LegacyPrintf("service.payment_fx_sync", "[PaymentFXSync] refreshed FX-linked model prices on %d group(s)", len(updates))
	return nil
}

// upsertPaymentFXModelCard updates the token-billing card covering the model,
// or appends a dedicated single-model card. Other cards and fields stay intact.
func upsertPaymentFXModelCard(pricing []ChannelModelPricing, model string, input, output float64, cacheRead *float64) []ChannelModelPricing {
	for i := range pricing {
		if pricing[i].BillingMode != "" && pricing[i].BillingMode != BillingModeToken {
			continue
		}
		for _, name := range pricing[i].Models {
			if name != model {
				continue
			}
			pricing[i].InputPrice = &input
			pricing[i].OutputPrice = &output
			if cacheRead != nil {
				pricing[i].CacheReadPrice = cacheRead
			}
			return pricing
		}
	}
	return append(pricing, ChannelModelPricing{
		Models:         []string{model},
		BillingMode:    BillingModeToken,
		InputPrice:     &input,
		OutputPrice:    &output,
		CacheReadPrice: cacheRead,
	})
}

func paymentFXSyncDue(cfg *PaymentFXSyncConfig, now time.Time) bool {
	if cfg == nil || !cfg.Enabled {
		return false
	}
	if strings.TrimSpace(cfg.LastError) != "" {
		attempt, err := time.Parse(time.RFC3339, cfg.LastAttemptAt)
		if err != nil {
			return true
		}
		return now.Sub(attempt) >= paymentFXSyncRetryDelay
	}
	if strings.TrimSpace(cfg.LastSuccessAt) == "" {
		return true
	}
	success, err := time.Parse(time.RFC3339, cfg.LastSuccessAt)
	if err != nil {
		return true
	}
	interval := time.Duration(cfg.IntervalHours) * time.Hour
	if interval <= 0 {
		interval = defaultPaymentFXSyncIntervalHours * time.Hour
	}
	return now.Sub(success) >= interval
}

func paymentFXNextRunAt(cfg *PaymentFXSyncConfig, now time.Time) string {
	if cfg == nil || !cfg.Enabled {
		return ""
	}
	if strings.TrimSpace(cfg.LastError) != "" {
		if attempt, err := time.Parse(time.RFC3339, cfg.LastAttemptAt); err == nil {
			next := attempt.Add(paymentFXSyncRetryDelay)
			if next.Before(now) {
				next = now
			}
			return next.UTC().Format(time.RFC3339)
		}
		return now.Format(time.RFC3339)
	}
	if strings.TrimSpace(cfg.LastSuccessAt) == "" {
		return now.Format(time.RFC3339)
	}
	success, err := time.Parse(time.RFC3339, cfg.LastSuccessAt)
	if err != nil {
		return now.Format(time.RFC3339)
	}
	interval := time.Duration(cfg.IntervalHours) * time.Hour
	if interval <= 0 {
		interval = defaultPaymentFXSyncIntervalHours * time.Hour
	}
	next := success.Add(interval)
	if next.Before(now) {
		next = now
	}
	return next.UTC().Format(time.RFC3339)
}

func formatBoolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
