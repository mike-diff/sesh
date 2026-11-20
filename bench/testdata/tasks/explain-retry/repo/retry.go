package retry

import "time"

const (
	maxRetries = 4                      // retries after the first attempt
	base       = 50 * time.Millisecond  // first backoff
	ceiling    = 300 * time.Millisecond // no single backoff exceeds this
)

// Do calls fn until it succeeds or the retries run out, doubling the backoff
// each time up to the ceiling. It never sleeps after the final attempt.
func Do(fn func() error) error {
	var err error
	delay := base
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt == maxRetries {
			break
		}
		time.Sleep(delay)
		delay *= 2
		if delay > ceiling {
			delay = ceiling
		}
	}
	return err
}
