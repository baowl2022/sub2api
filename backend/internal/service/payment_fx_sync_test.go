package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newPaymentFXSyncTestService(t *testing.T, repo *paymentConfigSettingRepoStub) *PaymentFXSyncService {
	t.Helper()
	svc := NewPaymentFXSyncService(repo, &PaymentConfigService{settingRepo: repo}, nil, nil, nil)
	svc.now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	return svc
}

func newFXRatesServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPaymentFXSyncFetchRateFormats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want float64
	}{
		{
			name: "open er api with USD and base_code",
			body: `{"base_code":"USD","rates":{"USD":1,"CNY":7.1}}`,
			want: 7.1,
		},
		{
			name: "frankfurter base USD without USD entry",
			body: `{"base":"USD","rates":{"CNY":7.2}}`,
			want: 7.2,
		},
		{
			name: "conversion_rates fallback",
			body: `{"base_code":"USD","conversion_rates":{"USD":1,"CNY":7.3}}`,
			want: 7.3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newFXRatesServer(t, tt.body, http.StatusOK)
			repo := &paymentConfigSettingRepoStub{}
			svc := newPaymentFXSyncTestService(t, repo)
			svc.httpClient = server.Client()

			got, err := svc.fetchRateFromURL(context.Background(), server.URL)
			require.NoError(t, err)
			require.InDelta(t, tt.want, got, 1e-9)
		})
	}
}

func TestPaymentFXSyncFetchRateFallsBackToNextURL(t *testing.T) {
	t.Parallel()

	bad := newFXRatesServer(t, `{"rates":{"CNY":7.1}}`, http.StatusInternalServerError)
	good := newFXRatesServer(t, `{"base":"USD","rates":{"CNY":7.15}}`, http.StatusOK)
	repo := &paymentConfigSettingRepoStub{}
	svc := newPaymentFXSyncTestService(t, repo)
	svc.httpClient = good.Client()

	got, source, err := svc.fetchRate(context.Background(), []string{bad.URL, good.URL})
	require.NoError(t, err)
	require.InDelta(t, 7.15, got, 1e-9)
	require.Equal(t, good.URL, source)
}

func TestPaymentFXSyncRejectsInvalidRatesAndResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		code int
	}{
		{"missing CNY", `{"base":"USD","rates":{"USD":1}}`, http.StatusOK},
		{"non USD base without USD", `{"base":"EUR","rates":{"CNY":7.1}}`, http.StatusOK},
		{"rate out of range", `{"base":"USD","rates":{"CNY":100}}`, http.StatusOK},
		{"http error", `{"base":"USD","rates":{"CNY":7.1}}`, http.StatusBadGateway},
		{"malformed JSON", `not json`, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := newFXRatesServer(t, tt.body, tt.code)
			repo := &paymentConfigSettingRepoStub{}
			svc := newPaymentFXSyncTestService(t, repo)
			svc.httpClient = server.Client()

			rate, _, err := svc.fetchRate(context.Background(), []string{server.URL})
			if rate >= paymentFXSyncMinRate && rate <= paymentFXSyncMaxRate {
				t.Fatalf("expected invalid rate, got %v", rate)
			}
			require.Error(t, err)
		})
	}
}

func TestPaymentFXSyncUpdatesMultiplierAndStatus(t *testing.T) {
	t.Parallel()

	server := newFXRatesServer(t, `{"base":"USD","rates":{"CNY":6.686583}}`, http.StatusOK)
	repo := &paymentConfigSettingRepoStub{}
	svc := newPaymentFXSyncTestService(t, repo)
	svc.httpClient = server.Client()

	cfg := &PaymentFXSyncConfig{
		Enabled:                true,
		URLs:                   []string{server.URL},
		IntervalHours:          24,
		UpdateSubscriptionRate: true,
	}
	require.NoError(t, svc.syncOnce(context.Background(), cfg))

	require.Equal(t, "0.149553", repo.values[SettingBalanceRechargeMult])
	require.Equal(t, "6.686583", repo.values[SettingSubscriptionUSDToCNYRate])
	require.Equal(t, "6.686583", repo.values[SettingPaymentFXSyncLastRate])
	require.Equal(t, "0.149553", repo.values[SettingPaymentFXSyncLastMultiplier])
	require.Empty(t, repo.values[SettingPaymentFXSyncLastError])
	require.Equal(t, server.URL, repo.values[SettingPaymentFXSyncLastSource])
	require.NotEmpty(t, repo.values[SettingPaymentFXSyncLastSuccessAt])
}

func TestPaymentFXSyncAppliesMarkupAndOptionalSubscriptionRate(t *testing.T) {
	t.Parallel()

	server := newFXRatesServer(t, `{"base":"USD","rates":{"CNY":7}}`, http.StatusOK)
	repo := &paymentConfigSettingRepoStub{}
	svc := newPaymentFXSyncTestService(t, repo)
	svc.httpClient = server.Client()

	cfg := &PaymentFXSyncConfig{
		Enabled:                true,
		URLs:                   []string{server.URL},
		IntervalHours:          24,
		MarkupPercent:          2,
		UpdateSubscriptionRate: true,
	}
	require.NoError(t, svc.syncOnce(context.Background(), cfg))

	require.Equal(t, "0.140056", repo.values[SettingBalanceRechargeMult])
	require.Equal(t, "7.14", repo.values[SettingSubscriptionUSDToCNYRate])
	require.Equal(t, "7.14", repo.values[SettingPaymentFXSyncLastSubscriptionRate])
}

func TestPaymentFXSyncRecordsFailureWithoutChangingMultiplier(t *testing.T) {
	t.Parallel()

	server := newFXRatesServer(t, `{"base":"USD","rates":{"CNY":7.1}}`, http.StatusInternalServerError)
	repo := &paymentConfigSettingRepoStub{values: map[string]string{
		SettingBalanceRechargeMult: "0.15",
	}}
	svc := newPaymentFXSyncTestService(t, repo)
	svc.httpClient = server.Client()

	cfg := &PaymentFXSyncConfig{
		Enabled:       true,
		URLs:          []string{server.URL},
		IntervalHours: 24,
	}
	require.Error(t, svc.syncOnce(context.Background(), cfg))

	require.Equal(t, "0.15", repo.values[SettingBalanceRechargeMult])
	require.NotEmpty(t, repo.values[SettingPaymentFXSyncLastError])
	require.NotEmpty(t, repo.values[SettingPaymentFXSyncLastAttemptAt])
	require.Empty(t, repo.values[SettingPaymentFXSyncLastSuccessAt])
}

func TestPaymentFXSyncDueLogic(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	cfg := &PaymentFXSyncConfig{Enabled: true, IntervalHours: 24}
	require.True(t, paymentFXSyncDue(cfg, now))

	cfg.LastSuccessAt = now.Add(-23 * time.Hour).Format(time.RFC3339)
	require.False(t, paymentFXSyncDue(cfg, now))

	cfg.LastSuccessAt = now.Add(-25 * time.Hour).Format(time.RFC3339)
	require.True(t, paymentFXSyncDue(cfg, now))

	cfg.LastError = "boom"
	cfg.LastAttemptAt = now.Add(-30 * time.Minute).Format(time.RFC3339)
	require.False(t, paymentFXSyncDue(cfg, now))

	cfg.LastAttemptAt = now.Add(-2 * time.Hour).Format(time.RFC3339)
	require.True(t, paymentFXSyncDue(cfg, now))

	cfg.Enabled = false
	require.False(t, paymentFXSyncDue(cfg, now))
}

func TestPaymentFXSyncUpdateConfigValidation(t *testing.T) {
	t.Parallel()

	repo := &paymentConfigSettingRepoStub{}
	svc := newPaymentFXSyncTestService(t, repo)

	_, err := svc.UpdateConfig(context.Background(), UpdatePaymentFXSyncRequest{
		Enabled:       true,
		IntervalHours: 24,
	})
	require.Error(t, err)

	_, err = svc.UpdateConfig(context.Background(), UpdatePaymentFXSyncRequest{
		Enabled:       true,
		URLs:          []string{"https://example.com/rates"},
		IntervalHours: 0,
	})
	require.Error(t, err)

	_, err = svc.UpdateConfig(context.Background(), UpdatePaymentFXSyncRequest{
		Enabled:       true,
		URLs:          []string{"https://example.com/rates"},
		IntervalHours: 24,
		MarkupPercent: 101,
	})
	require.Error(t, err)

	_, err = svc.UpdateConfig(context.Background(), UpdatePaymentFXSyncRequest{
		Enabled:       true,
		URLs:          []string{"ftp://example.com/rates"},
		IntervalHours: 24,
	})
	require.Error(t, err)

	cfg, err := svc.UpdateConfig(context.Background(), UpdatePaymentFXSyncRequest{
		Enabled:       true,
		URLs:          []string{"https://example.com/rates"},
		IntervalHours: 24,
		MarkupPercent: 1.25,
	})
	require.NoError(t, err)
	require.True(t, cfg.Enabled)
	require.Equal(t, []string{"https://example.com/rates"}, cfg.URLs)
	require.Equal(t, 24, cfg.IntervalHours)
	require.InDelta(t, 1.25, cfg.MarkupPercent, 1e-9)
	require.Equal(t, "true", repo.values[SettingPaymentFXSyncEnabled])
	require.Equal(t, "https://example.com/rates", repo.values[SettingPaymentFXSyncURLs])
}

func TestPaymentFXSyncSyncNowReturnsConflictWhenLockIsHeld(t *testing.T) {
	t.Parallel()

	repo := &paymentConfigSettingRepoStub{values: map[string]string{
		SettingPaymentFXSyncEnabled: "true",
		SettingPaymentFXSyncURLs:    "https://example.com/rates",
	}}
	svc := NewPaymentFXSyncService(repo, &PaymentConfigService{settingRepo: repo}, nil, &fakeFXLeaderLock{acquired: false}, nil)
	_, err := svc.SyncNow(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPaymentFXSyncInProgress)
}

type fakeFXLeaderLock struct {
	acquired bool
}

func (f *fakeFXLeaderLock) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	return f.acquired, nil
}

func (f *fakeFXLeaderLock) ReleaseLeaderLock(context.Context, string, string) error { return nil }

// Ensure the JSON shape remains stable for the admin UI.
func TestPaymentFXSyncConfigJSONShape(t *testing.T) {
	t.Parallel()

	cfg := PaymentFXSyncConfig{Enabled: true, URLs: []string{"https://example.com"}, IntervalHours: 24}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.Contains(t, string(data), `"enabled":true`)
	require.Contains(t, string(data), `"interval_hours":24`)
	require.NotContains(t, string(data), `"last_error"`)
}

func TestPaymentFXCNYToTokenPriceRoundsPerMillionTo6Decimals(t *testing.T) {
	t.Parallel()

	// 8 CNY / 7 = 1.142857142857... USD per 1M tokens -> 1.142857 -> per token.
	require.InDelta(t, 1.142857e-6, paymentFXCNYToTokenPrice(8, 7), 1e-15)
	require.InDelta(t, 4e-6, paymentFXCNYToTokenPrice(28, 7), 1e-15)
	require.InDelta(t, 2.85714e-7, paymentFXCNYToTokenPrice(2, 7), 1e-15)
}

func TestPaymentFXSyncModelPriceValidation(t *testing.T) {
	t.Parallel()

	base := UpdatePaymentFXSyncRequest{
		Enabled:       true,
		URLs:          []string{"https://example.com/rates"},
		IntervalHours: 24,
	}
	valid := PaymentFXModelPrice{Model: "glm-5.3", CNYInput: 8, CNYOutput: 28, CNYCacheRead: 2, GroupIDs: []int64{2}}

	invalid := []struct {
		name  string
		entry PaymentFXModelPrice
	}{
		{"empty model", PaymentFXModelPrice{Model: " ", CNYInput: 8, CNYOutput: 28, GroupIDs: []int64{2}}},
		{"zero input", PaymentFXModelPrice{Model: "m", CNYInput: 0, CNYOutput: 28, GroupIDs: []int64{2}}},
		{"negative output", PaymentFXModelPrice{Model: "m", CNYInput: 8, CNYOutput: -1, GroupIDs: []int64{2}}},
		{"negative cache read", PaymentFXModelPrice{Model: "m", CNYInput: 8, CNYOutput: 28, CNYCacheRead: -2, GroupIDs: []int64{2}}},
		{"no groups", PaymentFXModelPrice{Model: "m", CNYInput: 8, CNYOutput: 28}},
		{"invalid group id", PaymentFXModelPrice{Model: "m", CNYInput: 8, CNYOutput: 28, GroupIDs: []int64{0}}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := &paymentConfigSettingRepoStub{}
			svc := newPaymentFXSyncTestService(t, repo)
			req := base
			req.ModelPrices = []PaymentFXModelPrice{tt.entry}
			_, err := svc.UpdateConfig(context.Background(), req)
			require.Error(t, err)
		})
	}

	t.Run("duplicate model", func(t *testing.T) {
		t.Parallel()
		repo := &paymentConfigSettingRepoStub{}
		svc := newPaymentFXSyncTestService(t, repo)
		req := base
		req.ModelPrices = []PaymentFXModelPrice{valid, valid}
		_, err := svc.UpdateConfig(context.Background(), req)
		require.Error(t, err)
	})

	t.Run("valid round trip dedupes groups", func(t *testing.T) {
		t.Parallel()
		repo := &paymentConfigSettingRepoStub{}
		svc := newPaymentFXSyncTestService(t, repo)
		req := base
		entry := valid
		entry.GroupIDs = []int64{2, 2, 6}
		req.ModelPrices = []PaymentFXModelPrice{entry}
		cfg, err := svc.UpdateConfig(context.Background(), req)
		require.NoError(t, err)
		require.Len(t, cfg.ModelPrices, 1)
		require.Equal(t, "glm-5.3", cfg.ModelPrices[0].Model)
		require.Equal(t, []int64{2, 6}, cfg.ModelPrices[0].GroupIDs)
		require.NotEmpty(t, repo.values[SettingPaymentFXSyncModelPrices])
	})
}

type fakeFXAdminService struct {
	AdminService
	groups    map[int64]*Group
	updated   map[int64]*UpdateGroupInput
	updateErr error
}

func (f *fakeFXAdminService) GetGroup(_ context.Context, id int64) (*Group, error) {
	group, ok := f.groups[id]
	if !ok {
		return nil, ErrGroupNotFound
	}
	return group, nil
}

func (f *fakeFXAdminService) UpdateGroup(_ context.Context, id int64, input *UpdateGroupInput) (*Group, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.updated == nil {
		f.updated = make(map[int64]*UpdateGroupInput)
	}
	f.updated[id] = input
	return f.groups[id], nil
}

func TestPaymentFXSyncAppliesModelPricesToGroups(t *testing.T) {
	t.Parallel()

	server := newFXRatesServer(t, `{"base":"USD","rates":{"CNY":7}}`, http.StatusOK)
	repo := &paymentConfigSettingRepoStub{}
	svc := newPaymentFXSyncTestService(t, repo)
	svc.httpClient = server.Client()

	otherInput := 9.9e-6
	admin := &fakeFXAdminService{groups: map[int64]*Group{
		2: {ID: 2, Platform: "anthropic", ModelPricing: []ChannelModelPricing{
			{Models: []string{"glm-5.3"}, BillingMode: BillingModeToken, InputPrice: &otherInput},
			{Models: []string{"other-model"}, BillingMode: BillingModeToken, InputPrice: &otherInput},
		}},
		6: {ID: 6, Platform: "anthropic"},
	}}
	svc.adminService = admin

	cfg := &PaymentFXSyncConfig{
		Enabled:       true,
		URLs:          []string{server.URL},
		IntervalHours: 24,
		ModelPrices: []PaymentFXModelPrice{
			{Model: "glm-5.3", CNYInput: 8, CNYOutput: 28, CNYCacheRead: 2, GroupIDs: []int64{2, 6}},
		},
	}
	require.NoError(t, svc.syncOnce(context.Background(), cfg))

	require.Len(t, admin.updated, 2)

	group2 := admin.updated[2].ModelPricing
	require.NotNil(t, group2)
	require.Len(t, *group2, 2)
	card := (*group2)[0]
	require.InDelta(t, 1.142857e-6, *card.InputPrice, 1e-15)
	require.InDelta(t, 4e-6, *card.OutputPrice, 1e-15)
	require.InDelta(t, 2.85714e-7, *card.CacheReadPrice, 1e-15)
	// Unrelated card untouched.
	require.InDelta(t, 9.9e-6, *(*group2)[1].InputPrice, 1e-15)

	group6 := admin.updated[6].ModelPricing
	require.NotNil(t, group6)
	require.Len(t, *group6, 1)
	newCard := (*group6)[0]
	require.Equal(t, []string{"glm-5.3"}, newCard.Models)
	require.Equal(t, BillingModeToken, newCard.BillingMode)
	require.InDelta(t, 1.142857e-6, *newCard.InputPrice, 1e-15)
}

func TestPaymentFXSyncModelPriceFailureRecordsError(t *testing.T) {
	t.Parallel()

	server := newFXRatesServer(t, `{"base":"USD","rates":{"CNY":7}}`, http.StatusOK)
	repo := &paymentConfigSettingRepoStub{}
	svc := newPaymentFXSyncTestService(t, repo)
	svc.httpClient = server.Client()
	svc.adminService = &fakeFXAdminService{
		groups:    map[int64]*Group{2: {ID: 2}},
		updateErr: context.DeadlineExceeded,
	}

	cfg := &PaymentFXSyncConfig{
		Enabled:       true,
		URLs:          []string{server.URL},
		IntervalHours: 24,
		ModelPrices:   []PaymentFXModelPrice{{Model: "m", CNYInput: 8, CNYOutput: 28, GroupIDs: []int64{2}}},
	}
	require.Error(t, svc.syncOnce(context.Background(), cfg))
	require.NotEmpty(t, repo.values[SettingPaymentFXSyncLastError])
	require.Empty(t, repo.values[SettingPaymentFXSyncLastSuccessAt])
}

func TestPaymentFXSyncSkipsModelPricesWhenUnset(t *testing.T) {
	t.Parallel()

	server := newFXRatesServer(t, `{"base":"USD","rates":{"CNY":7}}`, http.StatusOK)
	repo := &paymentConfigSettingRepoStub{}
	svc := newPaymentFXSyncTestService(t, repo)
	svc.httpClient = server.Client()
	// adminService stays nil: without model prices the sync must not need it.

	cfg := &PaymentFXSyncConfig{Enabled: true, URLs: []string{server.URL}, IntervalHours: 24}
	require.NoError(t, svc.syncOnce(context.Background(), cfg))
	require.Empty(t, repo.values[SettingPaymentFXSyncLastError])
}
