// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package logreplayconnector

import (
	"context"
	"errors"
	"io/fs"
	"math/rand/v2"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

// scopeName is the name of the meter of the connector's own telemetry. The
// copies keep the scope of the original record.
const scopeName = "github.com/aws/amazon-cloudwatch-agent/connector/logreplayconnector"

// The first tick comes startupReplayDelay plus a random jitter in
// [0, min(interval, maxStartupJitter)) after Start. After a restart, the
// first tick sends the restored records, so the delay lets the exporters of
// the next pipeline connect first, and the jitter spreads the ticks of the
// agents of a cluster that start together.
const (
	startupReplayDelay = 10 * time.Second
	maxStartupJitter   = 10 * time.Second
)

// randomJitter returns a random duration in [0, n). n must be positive.
func randomJitter(n time.Duration) time.Duration {
	return rand.N(n) //nolint:gosec // G404: a start jitter does not need a secure random source.
}

// startReplaying launches the replay goroutine and returns at once.
func (c *replayConnector) startReplaying() {
	ctx, cancel := context.WithCancel(context.Background())
	delay := c.startupDelay + c.jitter(min(c.cfg.Interval, maxStartupJitter))
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.run(ctx, delay)
	}()
	c.stopReplay, c.replayDone = cancel, done
}

// cancelReplaying cancels the context of the replay and returns at once. It
// is safe to call before startReplaying and more than once.
func (c *replayConnector) cancelReplaying() {
	if c.stopReplay != nil {
		c.stopReplay()
	}
}

// waitReplaying waits for the replay to stop after cancelReplaying, bounded
// by ctx. It is safe to call before startReplaying and more than once.
func (c *replayConnector) waitReplaying(ctx context.Context) error {
	if c.replayDone == nil {
		return nil
	}
	select {
	case <-c.replayDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run ticks after delay, then every interval, until ctx is done.
func (c *replayConnector) run(ctx context.Context, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick sends a copy of every kept record whose source file still exists, in
// one call to the next consumer, and removes the records whose source file
// is gone. The entries are copied under the state lock, and the files are
// checked and the copies built and sent without it. An empty state sends
// nothing.
func (c *replayConnector) tick(ctx context.Context) {
	kept := c.state.entries()
	if len(kept) == 0 {
		return
	}
	ts := pcommon.NewTimestampFromTime(c.now())
	out := plog.NewLogs()
	sent := make([]int64, len(c.rules))
	var gone []keptEntry
	var skipped []skip
	for i := range kept {
		k := &kept[i]
		if k.entry.SourceFile != "" {
			if err := c.statFile(k.entry.SourceFile); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					gone = append(gone, *k)
				} else {
					// The record is not sent, because the connector cannot
					// tell whether its source still exists. It stays kept
					// for the next tick.
					skipped = append(skipped, skip{rule: k.rule, reason: skipSourceCheckError})
					c.logSourceError(c.rules[k.rule].cfg.Name, k.entry.SourceFile, err)
				}
				continue
			}
		}
		ld, err := decodeRecord(k.entry.Record)
		if err != nil {
			// Not expected: the connector encoded the record, and the
			// restore checks every stored record.
			skipped = append(skipped, skip{rule: k.rule, reason: skipDecodeError})
			if ce := c.logger.Check(zap.DebugLevel, "Kept log record not sent: it cannot be decoded"); ce != nil {
				ce.Write(zap.String("rule", c.rules[k.rule].cfg.Name), zap.Error(err))
			}
			continue
		}
		setTimes(ld, ts)
		ld.ResourceLogs().MoveAndAppendTo(out.ResourceLogs())
		sent[k.rule]++
	}

	if removed := c.state.removeGone(gone); len(removed) > 0 {
		c.changed()
		c.tel.recordRemovals(ctx, removed, nil)
		c.logRemovals(removed, 0)
	}
	c.tel.recordSkipped(ctx, skipped)

	if out.LogRecordCount() == 0 {
		return
	}
	err := c.next.ConsumeLogs(ctx, out)
	if err != nil && ctx.Err() != nil {
		if ce := c.logger.Check(zap.DebugLevel, "Log record copies not sent: the replay was canceled"); ce != nil {
			ce.Write(zap.Error(err))
		}
		return
	}
	c.tel.recordReplay(ctx, sent, err)
	if err != nil {
		c.logDownstreamError(err)
	}
}

// setTimes sets the timestamp and the observed timestamp of every record of
// ld to ts. Nothing else of a copy differs from the original.
func setTimes(ld plog.Logs, ts pcommon.Timestamp) {
	for _, rl := range ld.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			for _, lr := range sl.LogRecords().All() {
				lr.SetTimestamp(ts)
				lr.SetObservedTimestamp(ts)
			}
		}
	}
}

// logDownstreamError logs an error of the next consumer at most once per
// logInterval. The error does not go back to the logs pipeline that feeds
// the connector, and a tick has no caller to return it to.
func (c *replayConnector) logDownstreamError(err error) {
	suppressed, ok := c.downstreamErrLog.allow(c.now().UnixNano(), 1)
	if !ok {
		return
	}
	c.logger.Warn("Failed to send the log record copies to the next consumer",
		zap.Error(err),
		zap.Int64("suppressed_since_last_log", suppressed),
	)
}

// logSourceError logs a source file that could not be checked, at most once
// per logInterval.
func (c *replayConnector) logSourceError(rule, path string, err error) {
	const msg = "Kept log record not sent: its source file cannot be checked"
	if suppressed, ok := c.sourceErrLog.allow(c.now().UnixNano(), 1); ok {
		c.logger.Warn(msg,
			zap.String("rule", rule),
			zap.String("source_file", path),
			zap.Error(err),
			zap.Int64("suppressed_since_last_log", suppressed),
		)
		return
	}
	if ce := c.logger.Check(zap.DebugLevel, msg); ce != nil {
		ce.Write(zap.String("rule", rule), zap.String("source_file", path), zap.Error(err))
	}
}
