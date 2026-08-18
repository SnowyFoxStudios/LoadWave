// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command checkout is a load test written with the LoadWave Go SDK.
//
// It shows what the declarative YAML format cannot express: per-user login
// state, conditional flow, weighted decisions and a custom metric. Build it
// once and the resulting binary is a complete LoadWave node — it can run the
// test standalone, serve the dashboard, or join a coordinator as an agent:
//
//	go build -o checkout ./examples/checkout
//
//	./checkout run --url https://staging.example.com --vus 50 --duration 2m
//	./checkout run examples/checkout/checkout.yaml --ui
//
//	# Or spread it over several machines, using the same binary everywhere.
//	./checkout serve --listen 0.0.0.0:8090
//	./checkout agent --coordinator loadgen-controller:8090
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave/run"
)

func main() {
	loadwave.Register(loadwave.Scenario{
		Name:        "browse",
		Weight:      4,
		Description: "A visitor looking around without buying anything.",
		Run:         browse,
	})

	loadwave.Register(loadwave.Scenario{
		Name:        "checkout",
		Weight:      1,
		Description: "A signed-in customer completing a purchase.",
		OnVUStart:   signIn,
		Run:         checkout,
	})

	run.Main()
}

// stateKeyToken is where each virtual user keeps its session token.
const stateKeyToken = "token"

// signIn logs one virtual user in, once, before its first iteration.
//
// Doing this per user rather than per iteration is the point of OnVUStart: a
// real customer signs in once and then browses, and a test that re-authenticated
// on every iteration would spend most of its load on the login endpoint and
// measure that instead of the checkout flow.
func signIn(ctx context.Context, vu *loadwave.VU) error {
	// Each virtual user gets its own account, derived from its run-wide unique
	// id so that two workers — or two machines — never collide.
	credentials := map[string]string{
		"username": fmt.Sprintf("loadtest-user-%d", vu.ID()),
		"password": "correct-horse-battery-staple",
	}

	resp, err := vu.HTTP().Do(ctx, loadwave.Request{
		Method: http.MethodPost,
		URL:    "/api/auth/login",
		Name:   "POST /api/auth/login",
		JSON:   credentials,
	})
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login returned %d", resp.StatusCode)
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := resp.JSON(&body); err != nil {
		return fmt.Errorf("login response: %w", err)
	}
	if body.Token == "" {
		return errors.New("login returned no token")
	}

	vu.SetState(stateKeyToken, body.Token)
	return nil
}

// authHeader builds the header carrying this user's session.
func authHeader(vu *loadwave.VU) http.Header {
	token, _ := loadwave.StateOf[string](vu, stateKeyToken)
	return http.Header{"Authorization": []string{"Bearer " + token}}
}

// browse walks the catalogue the way an undecided visitor would.
func browse(ctx context.Context, vu *loadwave.VU) error {
	resp, err := vu.HTTP().Get(ctx, "/api/products?page=1")
	if err != nil {
		return err
	}
	if !vu.Check("catalogue loads", resp.StatusCode == http.StatusOK) {
		return fmt.Errorf("catalogue returned %d", resp.StatusCode)
	}

	var catalogue struct {
		Items []struct {
			ID int `json:"id"`
		} `json:"items"`
	}
	if err := resp.JSON(&catalogue); err != nil {
		return err
	}
	if !vu.Check("catalogue is not empty", len(catalogue.Items) > 0) {
		return nil
	}

	// Real people read the page before clicking. Jittered, because constant
	// think times make virtual users march in lockstep and produce traffic
	// spikes no real population would.
	vu.ThinkBetween(ctx, 500*time.Millisecond, 2*time.Second)

	// Look at a product chosen at random. The VU's generator is seeded from
	// its id, so the same run makes the same choices and a failure is
	// reproducible rather than a coin flip.
	pick := catalogue.Items[vu.Rand().IntN(len(catalogue.Items))]
	detail, err := vu.HTTP().Do(ctx, loadwave.Request{
		URL: fmt.Sprintf("/api/products/%d", pick.ID),
		// Set explicitly so every product shares one time series. Left to
		// the automatic naming this would still collapse, but being explicit
		// is what keeps cardinality predictable as URLs change.
		Name: "GET /api/products/{id}",
	})
	if err != nil {
		return err
	}
	vu.Check("product page loads", detail.StatusCode == http.StatusOK)

	vu.ThinkBetween(ctx, time.Second, 4*time.Second)
	return nil
}

// cartValue is a custom metric: the money each simulated basket is worth.
//
// Custom metrics are ordinary trend metrics, so this shows up in the dashboard
// with percentiles alongside the latency ones, and can carry a threshold.
const cartValue = "cart_value"

// checkout signs a purchase all the way through.
func checkout(ctx context.Context, vu *loadwave.VU) error {
	auth := authHeader(vu)

	// Tag everything this iteration emits, so the dashboard can separate a
	// slow checkout from a slow browse. Keep tag values to a small fixed set;
	// a tag per customer would create a time series per customer.
	vu.Tag("flow", "purchase")

	added, err := vu.HTTP().Do(ctx, loadwave.Request{
		Method:       http.MethodPost,
		URL:          "/api/cart/items",
		Name:         "POST /api/cart/items",
		Header:       auth,
		JSON:         map[string]any{"productId": 4711, "quantity": 1},
		ExpectStatus: []int{http.StatusCreated, http.StatusOK},
	})
	if err != nil {
		return err
	}
	if !vu.Checkf("item added to cart", added.OK(), "add to cart returned %d", added.StatusCode) {
		return fmt.Errorf("add to cart returned %d", added.StatusCode)
	}

	vu.ThinkBetween(ctx, time.Second, 3*time.Second)

	order, err := vu.HTTP().Do(ctx, loadwave.Request{
		Method: http.MethodPost,
		URL:    "/api/orders",
		Name:   "POST /api/orders",
		Header: auth,
		JSON:   map[string]any{"paymentMethod": "card"},
		// Checkout is slower than a page view and deserves its own budget
		// rather than being cut off by the run-wide default.
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return err
	}
	if !vu.Check("order accepted", order.StatusCode == http.StatusCreated) {
		return fmt.Errorf("order returned %d", order.StatusCode)
	}

	var receipt struct {
		Total float64 `json:"total"`
	}
	if err := order.JSON(&receipt); err != nil {
		return err
	}
	vu.Metrics().Trend(cartValue, vu.Labels(), receipt.Total)

	return nil
}
