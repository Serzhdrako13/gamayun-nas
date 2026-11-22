package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DemoSleepKind — тестовая задача-заглушка. Регистрируется только в тестах
// и в gamayund с флагом -demo-sleep; в обычном режиме недоступна.
const DemoSleepKind = "demo.sleep"

// DemoSleepParams — параметры demo.sleep.
type DemoSleepParams struct {
	DurationMS int  `json:"duration_ms"` // 1..600000
	Fail       bool `json:"fail,omitempty"`
}

// DemoSleep ждёт DurationMS, сообщая прогресс десятью шагами; уважает отмену.
func DemoSleep(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	var p DemoSleepParams
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("параметры demo.sleep: %w", err)
	}
	if p.DurationMS < 1 || p.DurationMS > 600_000 {
		return nil, errors.New("duration_ms должен быть 1..600000")
	}
	step := time.Duration(p.DurationMS) * time.Millisecond / 10
	for i := 1; i <= 10; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(step):
		}
		progress(i * 10)
	}
	if p.Fail {
		return nil, errors.New("заданный сбой demo.sleep")
	}
	return map[string]int{"slept_ms": p.DurationMS}, nil
}
