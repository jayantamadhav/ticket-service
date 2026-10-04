// Command burst reproduces an on-sale stampede against a running
// ticket-service instance: it creates a fresh show, then fires a large
// number of concurrent reservation requests at it — including a
// hot-seat storm where hundreds of distinct users all fight over the
// same single seat — and prints the outcome distribution plus a final
// reconciliation check (available + held + confirmed == total_seats).
//
// Usage:
//
//	go run ./cmd/burst -base-url http://localhost:8080
//	BASE_URL=https://my-deploy.example.com go run ./cmd/burst
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type outcome struct {
	confirmed    int64
	declined     map[string]*int64 // reason -> count
	status5xx    int64
	other        int64 // transport-level failure: no HTTP response was ever received
	otherSamples []string
	completed    int64 // total requests that got any outcome (confirmed+declined+5xx+other)
	mu           sync.Mutex
}

func newOutcome() *outcome {
	return &outcome{declined: make(map[string]*int64)}
}

// recordOther tracks a transport-level failure (connection refused, timeout,
// reset, etc. — anything where client.Do itself errored and no HTTP status
// was ever returned) and keeps a small sample of the actual error messages
// so a burst run doesn't silently hide *why* requests never completed.
func (o *outcome) recordOther(err error) {
	atomic.AddInt64(&o.other, 1)
	atomic.AddInt64(&o.completed, 1)
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.otherSamples) < 10 {
		o.otherSamples = append(o.otherSamples, err.Error())
	}
}

func (o *outcome) declinedCounter(reason string) *int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.declined[reason]
	if !ok {
		var zero int64
		c = &zero
		o.declined[reason] = c
	}
	return c
}

type showResponse struct {
	ID    string `json:"id"`
	Seats []struct {
		SeatLabel string `json:"seat_label"`
	} `json:"seats"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func main() {
	baseURL := flag.String("base-url", envOr("BASE_URL", "http://localhost:8080"), "ticket-service base URL")
	hotSeatStormers := flag.Int("hot-seat-users", 500, "number of distinct users storming the single hot seat")
	spreadUsers := flag.Int("spread-users", 2000, "number of distinct users booking random seats across the rest of the hall")
	totalSeats := flag.Int("seats", 500, "number of seats on the show (including the hot seat)")
	perUserLimit := flag.Int("per-user-limit", 4, "per_user_limit for the show")
	retryStormers := flag.Int("retry-users", 50, "number of users that fire duplicate (same idempotency key) retries")
	timeout := flag.Duration("timeout", 30*time.Second, "per-request HTTP timeout")
	maxInFlight := flag.Int("max-in-flight", 2000, "max requests in flight at once for the spread/limit/retry load (the hot-seat storm always fires fully simultaneously, uncapped, since that's the race it's testing)")
	flag.Parse()

	// The default http.Transport caps idle connections per host at 2, which
	// serializes most of a 20k-concurrent-goroutine burst onto a handful of
	// TCP connections and manifests as client-side timeouts that look like
	// server failures. Raise the ceiling so the client itself isn't the
	// bottleneck when we're deliberately trying to hammer the server.
	transport := &http.Transport{
		MaxIdleConns:        20000,
		MaxIdleConnsPerHost: 20000,
		MaxConnsPerHost:     0, // unlimited — we want every goroutine able to open its own connection
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Timeout: *timeout, Transport: transport}

	fmt.Printf("== ticket-service burst ==\nbase_url=%s seats=%d hot_seat_users=%d spread_users=%d retry_users=%d per_user_limit=%d\n\n",
		*baseURL, *totalSeats, *hotSeatStormers, *spreadUsers, *retryStormers, *perUserLimit)

	show, seatLabels, err := createShow(client, *baseURL, *totalSeats, *perUserLimit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create show: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("created show %s with %d seats\n", show, len(seatLabels))

	hotSeat := seatLabels[0]
	restOfHall := seatLabels[1:]

	totalRequests := *hotSeatStormers + *spreadUsers + 50 + *retryStormers*5

	o := newOutcome()
	var wg sync.WaitGroup

	// Prints a progress line every second, overwritten in place (\r, no
	// newline), so a long-running burst (especially against a real
	// deployment with network latency, not localhost) doesn't look hung
	// between the "firing..." lines and the final summary without spamming
	// the terminal with one line per tick.
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				done := atomic.LoadInt64(&o.completed)
				fmt.Printf("\r  ... %d/%d requests completed (confirmed=%d, 5xx=%d, other=%d)   ",
					done, totalRequests, atomic.LoadInt64(&o.confirmed),
					atomic.LoadInt64(&o.status5xx), atomic.LoadInt64(&o.other))
			case <-progressDone:
				fmt.Print("\r" + strings.Repeat(" ", 80) + "\r") // clear the progress line
				return
			}
		}
	}()

	// Bounds how many requests are in flight at once for the bulk load
	// below, so a 20k-request burst doesn't need 20k simultaneous OS
	// threads/fds on the machine generating it. Requests still queue up
	// and fire continuously — this caps concurrency, not throughput.
	sem := make(chan struct{}, *maxInFlight)
	submit := func(fn func()) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn()
		}()
	}

	// 1. Hot-seat storm: N distinct users all fight over the exact same
	// seat, fully simultaneously and uncapped — this is the one scenario
	// where true all-at-once concurrency is the point of the test. Checked
	// on its OWN wait group, immediately after it finishes, rather than
	// after the whole burst: holds expire after HoldTTL (45s), and a
	// 20k-request burst can easily run longer than that, which would make a
	// seat a storm correctly won look "available" again by the time we
	// finally check — a test-timing artifact, not a server bug.
	fmt.Printf("\nstorming hot seat %q with %d concurrent users...\n", hotSeat, *hotSeatStormers)
	var hotSeatWG sync.WaitGroup
	for i := 0; i < *hotSeatStormers; i++ {
		wg.Add(1)
		hotSeatWG.Add(1)
		go func(i int) {
			defer wg.Done()
			defer hotSeatWG.Done()
			userID := fmt.Sprintf("hotseat-user-%d", i)
			key := fmt.Sprintf("hotseat-%d-%d", i, time.Now().UnixNano())
			reserve(client, *baseURL, show, userID, key, []string{hotSeat}, o)
		}(i)
	}
	hotSeatWG.Wait()
	checkHotSeat(client, *baseURL, show, hotSeat, *hotSeatStormers)

	// 2. Background stampede: many users booking random seats elsewhere,
	// to exercise ordinary contention + per-user-limit enforcement.
	fmt.Printf("firing %d concurrent users across the rest of the hall (max %d in flight at once)...\n", *spreadUsers, *maxInFlight)
	for i := 0; i < *spreadUsers; i++ {
		i := i
		submit(func() {
			userID := fmt.Sprintf("spread-user-%d", i)
			key := fmt.Sprintf("spread-%d-%d", i, time.Now().UnixNano())
			n := 1 + rand.Intn(3) // 1-3 seats per request, exercises partial/all-or-nothing path
			seats := pickRandomSeats(restOfHall, n)
			reserve(client, *baseURL, show, userID, key, seats, o)
		})
	}

	// 3. Per-user limit stress: a handful of single users each fire 10
	// parallel reserves against a limit=perUserLimit show.
	fmt.Printf("stressing per-user limit with 10 parallel reserves each, for 5 users...\n")
	for u := 0; u < 5; u++ {
		userID := fmt.Sprintf("limit-user-%d", u)
		for i := 0; i < 10; i++ {
			i := i
			submit(func() {
				key := fmt.Sprintf("%s-limit-%d-%d", userID, i, time.Now().UnixNano())
				seats := pickRandomSeats(restOfHall, 1)
				reserve(client, *baseURL, show, userID, key, seats, o)
			})
		}
	}

	// 4. Idempotent retries: same user, same idempotency key, fired
	// concurrently multiple times — must collapse to exactly one reservation.
	fmt.Printf("firing idempotent retries for %d users (same key, same body, concurrent)...\n", *retryStormers)
	for i := 0; i < *retryStormers; i++ {
		userID := fmt.Sprintf("retry-user-%d", i)
		key := fmt.Sprintf("retry-key-%d", i)
		seats := pickRandomSeats(restOfHall, 1)
		for r := 0; r < 5; r++ {
			submit(func() {
				reserve(client, *baseURL, show, userID, key, seats, o)
			})
		}
	}

	wg.Wait()
	close(progressDone)

	fmt.Printf("\nall %d requests completed.\n", atomic.LoadInt64(&o.completed))

	fmt.Println("\n== outcome distribution ==")
	fmt.Printf("confirmed:  %d\n", atomic.LoadInt64(&o.confirmed))
	o.mu.Lock()
	for reason, count := range o.declined {
		fmt.Printf("declined[%s]: %d\n", reason, atomic.LoadInt64(count))
	}
	o.mu.Unlock()
	fmt.Printf("5xx:        %d\n", atomic.LoadInt64(&o.status5xx))
	fmt.Printf("other:      %d (transport-level failures — no HTTP response received)\n", atomic.LoadInt64(&o.other))
	if len(o.otherSamples) > 0 {
		fmt.Println("other samples (first 10 distinct causes seen):")
		for _, s := range o.otherSamples {
			fmt.Printf("  - %s\n", s)
		}
	}

	fmt.Println("\n== final reconciliation ==")
	state, err := getShowState(client, *baseURL, show)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to fetch final show state: %v\n", err)
		os.Exit(1)
	}
	sum := state.Counts.Available + state.Counts.Held + state.Counts.Confirmed
	fmt.Printf("available=%d held=%d confirmed=%d total=%d sum=%d (expected total=%d)\n",
		state.Counts.Available, state.Counts.Held, state.Counts.Confirmed, state.Counts.Total, sum, *totalSeats)

	if sum != *totalSeats {
		fmt.Println("RECONCILIATION FAILED: available+held+confirmed != total_seats")
		os.Exit(1)
	}
	if atomic.LoadInt64(&o.status5xx) != 0 {
		fmt.Println("CORRECTNESS FAILED: saw 5xx responses during the burst")
		os.Exit(1)
	}
	fmt.Println("\nreconciliation OK, hot seat has exactly one holder, zero 5xx")
}

// checkHotSeat verifies the hot seat resolved to exactly one holder,
// immediately after the storm (not after the whole burst) so hold-expiry
// (HoldTTL, 45s) can't make a correctly-won seat look unclaimed just because
// the rest of a large burst took a while to finish.
func checkHotSeat(client *http.Client, baseURL, showID, hotSeat string, stormers int) {
	fmt.Println("\n== hot-seat check (right after the storm) ==")
	state, err := getShowState(client, baseURL, showID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to fetch show state for hot-seat check: %v\n", err)
		os.Exit(1)
	}
	hotSeatStatus := "MISSING"
	for _, s := range state.Seats {
		if s.Label == hotSeat {
			hotSeatStatus = s.Status
			break
		}
	}
	fmt.Printf("seat %q status: %s (exactly one of the %d stormers should have won it)\n",
		hotSeat, hotSeatStatus, stormers)
	if hotSeatStatus != "held" && hotSeatStatus != "confirmed" {
		fmt.Println("HOT-SEAT CHECK FAILED: hot seat is not held/confirmed by anyone right after the storm")
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func pickRandomSeats(pool []string, n int) []string {
	if n > len(pool) {
		n = len(pool)
	}
	idx := rand.Perm(len(pool))[:n]
	seats := make([]string, n)
	for i, p := range idx {
		seats[i] = pool[p]
	}
	return seats
}

func createShow(client *http.Client, baseURL string, totalSeats, perUserLimit int) (showID string, seatLabels []string, err error) {
	seatLabels = make([]string, totalSeats)
	for i := 0; i < totalSeats; i++ {
		seatLabels[i] = fmt.Sprintf("S%d", i+1)
	}

	body, _ := json.Marshal(map[string]any{
		"name":           fmt.Sprintf("burst-%d", time.Now().UnixNano()),
		"seats":          seatLabels,
		"price_paise":    25000,
		"per_user_limit": perUserLimit,
	})

	resp, err := client.Post(baseURL+"/shows", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("unexpected status %d creating show: %s", resp.StatusCode, b)
	}

	var sr showResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", nil, err
	}
	return sr.ID, seatLabels, nil
}

func reserve(client *http.Client, baseURL, showID, userID, idempotencyKey string, seats []string, o *outcome) {
	body, _ := json.Marshal(map[string]any{
		"seats":           seats,
		"idempotency_key": idempotencyKey,
	})

	req, err := http.NewRequest(http.MethodPost, baseURL+"/shows/"+showID+"/reserve", bytes.NewReader(body))
	if err != nil {
		o.recordOther(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userID)

	resp, err := client.Do(req)
	if err != nil {
		o.recordOther(err)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusCreated:
		atomic.AddInt64(&o.confirmed, 1)
		atomic.AddInt64(&o.completed, 1)
	case resp.StatusCode >= 500:
		atomic.AddInt64(&o.status5xx, 1)
		atomic.AddInt64(&o.completed, 1)
	case resp.StatusCode >= 400:
		var er errorResponse
		reason := fmt.Sprintf("http_%d", resp.StatusCode)
		if err := json.NewDecoder(resp.Body).Decode(&er); err == nil && er.Error != "" {
			reason = er.Error
		}
		atomic.AddInt64(o.declinedCounter(reason), 1)
		atomic.AddInt64(&o.completed, 1)
	default:
		o.recordOther(fmt.Errorf("unexpected status %d", resp.StatusCode))
	}
}

type showState struct {
	Seats []struct {
		Label  string `json:"label"`
		Status string `json:"status"`
	} `json:"seats"`
	Counts struct {
		Available int `json:"available"`
		Held      int `json:"held"`
		Confirmed int `json:"confirmed"`
		Total     int `json:"total"`
	} `json:"counts"`
}

func getShowState(client *http.Client, baseURL, showID string) (*showState, error) {
	resp, err := client.Get(baseURL + "/shows/" + showID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, b)
	}

	var state showState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, err
	}
	return &state, nil
}
