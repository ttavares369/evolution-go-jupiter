package queuev1

import "time"

const deadlineLayout = "2006-01-02T15:04:05.000Z"

func canonicalDeadline(value string) bool {
	if value == "" {
		return true
	}
	t, err := time.Parse(deadlineLayout, value)
	return err == nil && len(value) == 24 && t.Format(deadlineLayout) == value
}

func deadlineExpired(value string, now func() time.Time) bool {
	if value == "" {
		return false
	}
	t, err := time.Parse(deadlineLayout, value)
	if err != nil {
		return true // Internal callers cannot turn an invalid deadline into infinity.
	}
	if now == nil {
		now = time.Now
	}
	return !now().Before(t)
}
