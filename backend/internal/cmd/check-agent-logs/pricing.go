// Offline pricing coverage warnings from replayed task costs.

package main

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/history"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

type pricingKey struct {
	harness harness.Name
	model   string
}

type pricingExample struct {
	path string
	at   time.Time
}

type priceAudit struct {
	pricer   *usage.Pricer
	mu       sync.Mutex
	warnings map[pricingKey]pricingExample
}

func newPriceAudit() *priceAudit {
	return &priceAudit{pricer: usage.NewPricer(nil), warnings: make(map[pricingKey]pricingExample)}
}

func (a *priceAudit) observe(lt *history.LoadedTask, path string) {
	c := task.CostTracker{Harness: lt.Harness, Model: lt.RequestedModel}
	p := missingPrices{pricer: a.pricer, models: make(map[string]time.Time)}
	for _, m := range lt.Timeline {
		if u, ok := m.Message.(*agent.UsageMessage); ok && u.Usage.Equal(agent.Usage{}) {
			c.Observe(m.Message, m.ProducerTime, a.pricer)
			continue
		}
		c.Observe(m.Message, m.ProducerTime, &p)
	}
	// A later harness total can cover usage whose initial estimate was missing.
	if c.Snapshot().UnknownParts == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for model, at := range p.models {
		key := pricingKey{harness: lt.Harness, model: model}
		// Logs finish in any order; the first path in sorted order is the example.
		if ex, ok := a.warnings[key]; !ok || path < ex.path {
			a.warnings[key] = pricingExample{path: path, at: at}
		}
	}
}

func (a *priceAudit) writeWarnings(out io.Writer) error {
	keys := slices.Collect(maps.Keys(a.warnings))
	slices.SortFunc(keys, func(a, b pricingKey) int {
		if a.harness != b.harness {
			return cmp.Compare(a.harness, b.harness)
		}
		return cmp.Compare(a.model, b.model)
	})
	for _, key := range keys {
		ex := a.warnings[key]
		if _, err := fmt.Fprintf(out, "%s: warning: missing offline price for model=%q harness=%s at %s in a log with incomplete cost\n", ex.path, key.model, key.harness, ex.at.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}

type missingPrices struct {
	pricer *usage.Pricer
	models map[string]time.Time
}

func (p *missingPrices) ModelPrice(model string, at time.Time) (usage.ModelPrice, bool) {
	price, ok := p.pricer.ModelPrice(model, at)
	if !ok {
		if _, seen := p.models[model]; !seen {
			p.models[model] = at
		}
	}
	return price, ok
}
