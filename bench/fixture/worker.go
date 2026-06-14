package main

import (
	"log"
	"time"
)

// Job is one async notification. Attempt starts at 1 and doubles the backoff
// each retry; after maxAttempts the job goes to the dead letter log and is
// dropped, never retried again.
type Job struct {
	Kind     string
	TicketID int64
	Attempt  int
}

const maxAttempts = 5

// RunWorker drains the queue forever. Delivery is at-least-once: a job that
// fails is re-enqueued with backoff, so downstream consumers must dedupe on
// (Kind, TicketID). The worker never blocks the HTTP path; when the queue is
// full the producer drops the notification and logs it instead.
func RunWorker(q chan Job, st *Store, base time.Duration) {
	for job := range q {
		if err := deliver(job, st); err != nil {
			if job.Attempt >= maxAttempts {
				log.Printf("DEAD LETTER %s ticket=%d after %d attempts: %v", job.Kind, job.TicketID, job.Attempt, err)
				continue
			}
			backoff := base * time.Duration(1<<job.Attempt)
			job.Attempt++
			time.AfterFunc(backoff, func() { q <- job })
			continue
		}
	}
}

func deliver(job Job, st *Store) error {
	if _, ok := st.Get(job.TicketID); !ok {
		return nil // ticket vanished; nothing to notify about
	}
	// Real delivery would POST a webhook here; the fixture just logs.
	log.Printf("delivered %s for ticket %d (attempt %d)", job.Kind, job.TicketID, job.Attempt)
	return nil
}
