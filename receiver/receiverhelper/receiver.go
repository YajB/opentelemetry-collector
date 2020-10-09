// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package receiverhelper

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenterror"
	"go.opentelemetry.io/collector/config/configmodels"
	"go.opentelemetry.io/collector/consumer"
)

// Start specifies the function invoked when the receiver is being started.
type Start func(context.Context, component.Host) error

// Shutdown specifies the function invoked when the receiver is being shutdown.
type Shutdown func(context.Context) error

// Option apply changes to internal options.
type Option func(*baseReceiver)

// WithStart overrides the default Start function for a receiver.
// The default shutdown function does nothing and always returns nil.
func WithStart(start Start) Option {
	return func(o *baseReceiver) {
		o.start = start
	}
}

// WithShutdown overrides the default Shutdown function for a receiver.
// The default shutdown function does nothing and always returns nil.
func WithShutdown(shutdown Shutdown) Option {
	return func(o *baseReceiver) {
		o.shutdown = shutdown
	}
}

type baseReceiver struct {
	fullName string
	start    Start
	shutdown Shutdown
}

// Construct the internalOptions from multiple Option.
func newBaseReceiver(fullName string, options ...Option) baseReceiver {
	br := baseReceiver{fullName: fullName}

	for _, op := range options {
		op(&br)
	}

	return br
}

// Start the receiver, invoked during service start.
func (br *baseReceiver) Start(ctx context.Context, host component.Host) error {
	if br.start != nil {
		return br.start(ctx, host)
	}
	return nil
}

// Shutdown the receiver, invoked during service shutdown.
func (br *baseReceiver) Shutdown(ctx context.Context) error {
	if br.shutdown != nil {
		return br.shutdown(ctx)
	}
	return nil
}

// MetricOption apply changes to internal options.
type MetricOption func(*metricsReceiver)

// WithBaseOptions applies any base options to a metrics receiver.
func WithBaseOptions(options ...Option) MetricOption {
	return func(o *metricsReceiver) {
		for _, option := range options {
			option(&o.baseReceiver)
		}
	}
}

// WithDefaultCollectionInterval overrides the default collection
// interval (1 minute) that will be applied to all scrapers if not
// overridden by the individual scraper.
func WithDefaultCollectionInterval(defaultCollectionInterval time.Duration) MetricOption {
	return func(o *metricsReceiver) {
		o.defaultCollectionInterval = defaultCollectionInterval
	}
}

// AddScraper configures the provided scrape function to be called with
// the specified options, and at the specified collection interval (one
// minute by default).
//
// Observability information will be reported, and the scraped metrics
// will be passed to the next consumer.
func AddScraper(cfg ScraperConfig, scrape Scrape, options ...ScraperOption) MetricOption {
	return func(o *metricsReceiver) {
		o.scrapers = append(o.scrapers, newScraper(cfg, scrape, options...))
	}
}

type metricsReceiver struct {
	baseReceiver
	defaultCollectionInterval time.Duration
	nextConsumer              consumer.MetricsConsumer

	scrapers []*scraper
	done     chan struct{}
}

// NewMetricReceiver creates a Receiver with the configured options.
func NewMetricReceiver(config configmodels.Receiver, nextConsumer consumer.MetricsConsumer, options ...MetricOption) (component.Receiver, error) {
	if nextConsumer == nil {
		return nil, componenterror.ErrNilNextConsumer
	}

	mr := &metricsReceiver{
		baseReceiver:              newBaseReceiver(config.Name()),
		defaultCollectionInterval: time.Minute,
		nextConsumer:              nextConsumer,
		done:                      make(chan struct{}),
	}

	for _, op := range options {
		op(mr)
	}

	// wrap the start function with a call to initialize scrapers
	// and start scraping
	start := mr.start
	mr.start = func(ctx context.Context, host component.Host) error {
		if start != nil {
			if err := start(ctx, host); err != nil {
				return err
			}
		}

		if err := mr.initializeScrapers(ctx); err != nil {
			return err
		}

		mr.startScraping()
		return nil
	}

	// wrap the shutdown function with a call to close scrapers
	// and stop scraping
	shutdown := mr.shutdown
	mr.shutdown = func(ctx context.Context) error {
		mr.stopScraping()

		var errors []error

		if err := mr.closeScrapers(ctx); err != nil {
			errors = append(errors, err)
		}

		if shutdown != nil {
			if err := shutdown(ctx); err != nil {
				errors = append(errors, err)
			}
		}

		return componenterror.CombineErrors(errors)
	}

	return mr, nil
}

// initializeScrapers initializes all the scrapers
func (mr *metricsReceiver) initializeScrapers(ctx context.Context) error {
	for _, scraper := range mr.scrapers {
		if scraper.initialize == nil {
			continue
		}

		if err := scraper.initialize(ctx); err != nil {
			return err
		}
	}

	return nil
}

// startScraping initiates a ticker that calls Scrape based on the configured
// collection interval.
func (mr *metricsReceiver) startScraping() {
	// TODO1: use one ticker for each set of scrapers that have the same collection interval.
	// TODO2: consider allowing different "Scrape" functions to be configured, i.e. functions
	//        that return MetricsSlice or ResourceMetricsSlice (similar to the existing Scraper
	//        & ResourceScraper interfaces in the host metrics receiver). That will allow data
	//        from multiple scrapers (that have the same collection interval) to be batched.

	for i := 0; i < len(mr.scrapers); i++ {
		scraper := mr.scrapers[i]
		go func() {
			collectionInterval := mr.defaultCollectionInterval
			if scraper.cfg.CollectionInterval() != 0 {
				collectionInterval = scraper.cfg.CollectionInterval()
			}

			ticker := time.NewTicker(collectionInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					mr.scrapeAndReport(context.Background(), scraper)
				case <-mr.done:
					return
				}
			}
		}()
	}
}

// scrapeAndReport calls the Scrape function of the provided Scraper, records
// observability information, and passes the scraped metrics to the next component.
func (mr *metricsReceiver) scrapeAndReport(ctx context.Context, scraper *scraper) {
	// TODO: Add observability metrics support
	metrics, err := scraper.scrape(ctx)
	if err != nil {
		return
	}

	mr.nextConsumer.ConsumeMetrics(ctx, metrics)
}

// stopScraping stops the ticker
func (mr *metricsReceiver) stopScraping() {
	close(mr.done)
}

// closeScrapers closes all the scrapers
func (mr *metricsReceiver) closeScrapers(ctx context.Context) error {
	var errors []error

	for _, scraper := range mr.scrapers {
		if scraper.close == nil {
			continue
		}

		if err := scraper.close(ctx); err != nil {
			errors = append(errors, err)
		}
	}

	return componenterror.CombineErrors(errors)
}