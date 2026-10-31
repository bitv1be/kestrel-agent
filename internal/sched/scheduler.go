package sched

import (
	"context"
	"log/slog"
	"time"

	"github.com/bitvibe/kestrel/internal/store"
)

// Runner enqueues a scheduled prompt for delivery.
type Runner interface {
	SubmitScheduled(ctx context.Context, chatID int64, prompt string) error
}

// Scheduler runs a persisted cron job list.
type Scheduler struct {
	Store  *store.Store
	Runner Runner
	Tick   time.Duration
	Log    *slog.Logger
}

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.Store == nil || s.Runner == nil {
		return nil
	}
	if s.Tick <= 0 {
		s.Tick = 30 * time.Second
	}
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	s.normalize(ctx, log)
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			s.tick(ctx, now, log)
		}
	}
}

func (s *Scheduler) normalize(ctx context.Context, log *slog.Logger) {
	jobs, err := s.Store.ListCronJobs(ctx, 0)
	if err != nil {
		log.Warn("scheduler: list jobs", "error", err)
		return
	}
	now := time.Now()
	for _, j := range jobs {
		if !j.Enabled || !j.NextRun.IsZero() && j.NextRun.After(now) {
			continue
		}
		schedule, err := Parse(j.Schedule)
		if err != nil {
			log.Warn("scheduler: invalid schedule", "id", j.ID, "schedule", j.Schedule, "error", err)
			continue
		}
		next := schedule.Next(now)
		if err := s.Store.MarkCronRun(ctx, j.ID, j.LastRun, next); err != nil {
			log.Warn("scheduler: update next run", "id", j.ID, "error", err)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context, now time.Time, log *slog.Logger) {
	jobs, err := s.Store.ListCronJobs(ctx, 0)
	if err != nil {
		log.Warn("scheduler: list jobs", "error", err)
		return
	}
	for _, j := range jobs {
		if !j.Enabled || j.NextRun.IsZero() || j.NextRun.After(now) {
			continue
		}
		schedule, err := Parse(j.Schedule)
		if err != nil {
			log.Warn("scheduler: invalid schedule", "id", j.ID, "schedule", j.Schedule, "error", err)
			continue
		}
		next := schedule.Next(now)
		// Persist before running so a crash cannot cause a duplicate storm.
		if err := s.Store.MarkCronRun(ctx, j.ID, now, next); err != nil {
			log.Warn("scheduler: mark run", "id", j.ID, "error", err)
			continue
		}
		if err := s.Runner.SubmitScheduled(ctx, j.ChatID, j.Prompt); err != nil {
			log.Warn("scheduler: submit", "id", j.ID, "error", err)
			continue
		}
		log.Info("scheduler: ran job", "id", j.ID, "chat_id", j.ChatID, "next", next)
	}
}
