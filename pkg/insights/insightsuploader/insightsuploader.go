package insightsuploader

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"time"

	"k8s.io/klog"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/support-operator/pkg/authorizer"
	"github.com/openshift/support-operator/pkg/controllerstatus"
	"github.com/openshift/support-operator/pkg/insights/insightsclient"
)

type Authorizer interface {
	Enabled() bool
	IsAuthorizationError(error) bool
}

type Summarizer interface {
	Summary(ctx context.Context, since time.Time) (io.ReadCloser, bool, error)
}

type StatusReporter interface {
	LastReportedTime() time.Time
	SetLastReportedTime(time.Time)
}

type Controller struct {
	controllerstatus.Simple

	summarizer Summarizer
	client     *insightsclient.Client
	reporter   StatusReporter
	interval   time.Duration
}

func New(summarizer Summarizer, client *insightsclient.Client, statusReporter StatusReporter, interval time.Duration) *Controller {
	return &Controller{
		Simple: controllerstatus.Simple{Name: "insightsuploader"},

		summarizer: summarizer,
		client:     client,
		reporter:   statusReporter,
		interval:   interval,
	}
}

func (c *Controller) Run(ctx context.Context) {
	// the controller periodically uploads results to the remote support endpoint
	interval := c.interval

	// TODO: when config is driven by an informer, we need to refactor this to be provided by the
	// config source (an edge triggered change that resets the loop)
	enabledCh := make(chan struct{}, 2)
	if c.client != nil {
		// load the initial interval
		initialEnabled, nextInterval, _ := c.client.Enabled()
		if nextInterval > 0 {
			interval = nextInterval
		}
		// every time the enabled state changes, attempt to wake the reporting loop
		go func() {
			for {
				enabled, _, _ := c.client.Enabled()
				if initialEnabled != enabled {
					select {
					case enabledCh <- struct{}{}:
					default:
					}
					initialEnabled = enabled
				}
				time.Sleep(2 * time.Minute)
			}
		}()
	}

	initialDelay := wait.Jitter(interval/8, 2)
	lastReported := c.reporter.LastReportedTime()
	if !lastReported.IsZero() {
		next := lastReported.Add(interval)
		if now := time.Now(); next.After(now) {
			initialDelay = wait.Jitter(now.Sub(next), 1.2)
		}
	}
	klog.V(2).Infof("Reporting status periodically to %s every %s, starting in %s", c.client.Endpoint(), interval, initialDelay.Truncate(time.Second))

	wait.Until(func() {
		if initialDelay > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(initialDelay):
			case <-enabledCh:
				klog.V(2).Infof("Reporting was enabled")
			}
			initialDelay = 0
		}

		// allow the support operator reporting to be enabled and disabled dynamically
		var enabled bool
		var disabledReason string
		var disabledMessage string
		if c.client != nil {
			var nextInterval time.Duration
			enabled, nextInterval, disabledMessage = c.client.Enabled()
			if nextInterval > 0 {
				interval = nextInterval
			} else {
				interval = c.interval
			}
		} else {
			disabledReason = "NotConfigured"
			disabledMessage = "Reporting has been disabled"
		}

		// attempt to get a summary to send to the server
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		source, ok, err := c.summarizer.Summary(ctx, lastReported)
		if err != nil {
			c.Simple.UpdateStatus(controllerstatus.Summary{Reason: "SummaryFailed", Message: fmt.Sprintf("Unable to retrieve local support data: %v", err)})
			return
		}
		if !ok {
			klog.V(4).Infof("Nothing to report since %s", lastReported.Format(time.RFC3339))
			return
		}
		defer source.Close()

		if enabled {
			// send the results
			start := time.Now()
			id := start.Format(time.RFC3339)
			klog.V(4).Infof("Uploading latest report since %s", lastReported.Format(time.RFC3339))
			if err := c.client.Send(ctx, insightsclient.Source{
				ID:       id,
				Type:     "application/vnd.redhat.openshift.periodic",
				Contents: source,
			}); err != nil {
				if err == insightsclient.ErrWaitingForVersion {
					initialDelay = wait.Jitter(interval/8, 1) - interval/8
					return
				}
				if authorizer.IsAuthorizationError(err) {
					c.Simple.UpdateStatus(controllerstatus.Summary{Reason: "NotAuthorized", Message: fmt.Sprintf("Uploading support data was not allowed: %v", err)})
					initialDelay = wait.Jitter(interval, 3)
					return
				}

				initialDelay = wait.Jitter(interval/8, 1.2)
				c.Simple.UpdateStatus(controllerstatus.Summary{Reason: "UploadFailed", Message: fmt.Sprintf("Unable to upload support data: %v", err)})
				return
			}

			klog.V(4).Infof("Uploaded report successfully in %s", time.Now().Sub(start))
			lastReported = start.UTC()
			c.reporter.SetLastReportedTime(lastReported)
			c.Simple.UpdateStatus(controllerstatus.Summary{Healthy: true})

		} else {
			klog.V(4).Info("Display report that would be sent")
			// display what would have been sent (to ensure we always exercise source processing)
			if err := reportToLogs(source, klog.V(4)); err != nil {
				klog.Errorf("Unable to log upload: %v", err)
			}
			// we didn't actually report logs, so don't advance the report date
			c.reporter.SetLastReportedTime(lastReported)

			if len(disabledReason) == 0 {
				disabledReason = "Disabled"
			}
			if len(disabledMessage) == 0 {
				disabledMessage = "Uploading reports has been disabled"
			}
			c.Simple.UpdateStatus(controllerstatus.Summary{Disabled: true, Reason: disabledReason, Message: disabledMessage})
		}

		initialDelay = wait.Jitter(interval, 1.2)
	}, 15*time.Second, ctx.Done())
}

// Init reports the initial state of the controller.
func (c *Controller) Init() {
	var enabled bool
	var disabledReason string
	var disabledMessage string
	if c.client != nil {
		enabled, _, disabledMessage = c.client.Enabled()
	} else {
		disabledReason = "NotConfigured"
		disabledMessage = "Reporting has been disabled"
	}

	if enabled {
		c.Simple.UpdateStatus(controllerstatus.Summary{Healthy: true})
		return
	}

	if len(disabledReason) == 0 {
		disabledReason = "Disabled"
	}
	if len(disabledMessage) == 0 {
		disabledMessage = "Uploading reports has been disabled"
	}

	c.Simple.UpdateStatus(controllerstatus.Summary{Disabled: true, Reason: disabledReason, Message: disabledMessage})
}

func reportToLogs(source io.Reader, klog klog.Verbose) error {
	if !klog {
		return nil
	}
	gr, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		klog.Infof("Dry-run: %s %7d %s", hdr.ModTime.Format(time.RFC3339), hdr.Size, hdr.Name)
	}
	return nil
}
