package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"time"

	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/storage/home"
)

// Params — параметры операции. Validate проверяет значения после строгого
// разбора JSON (неизвестные поля к этому моменту уже отклонены).
type Params interface {
	Validate() error
}

// NoParams — для операций без параметров: допустимы только отсутствие
// params, null или {}.
type NoParams struct{}

// Validate всегда успешна: лишние поля отсекает строгий разбор.
func (NoParams) Validate() error { return nil }

// Op — операция реестра.
type Op struct {
	Name    string
	Timeout time.Duration
	// Mutating — операция меняет систему (журналируется отдельно; взаимное
	// исключение — внутри исполнителя, см. home.Executor).
	Mutating bool
	call     func(ctx context.Context, params json.RawMessage) (any, error)
}

// ParamError — ошибка проверки параметров (код invalid_params).
type ParamError struct{ Err error }

func (e *ParamError) Error() string { return e.Err.Error() }
func (e *ParamError) Unwrap() error { return e.Err }

var opName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Define создаёт операцию с типизированными параметрами P.
// P — структура (не указатель); разбор строгий: неизвестные поля, лишние
// данные после объекта и значения не того типа отклоняются.
func Define[P Params](name string, timeout time.Duration, h func(ctx context.Context, p P) (any, error)) Op {
	if !opName.MatchString(name) {
		panic("helper: недопустимое имя операции " + name)
	}
	if timeout <= 0 {
		panic("helper: операция без таймаута " + name)
	}
	return Op{Name: name, Timeout: timeout, call: func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p P
		if err := decodeStrict(raw, &p); err != nil {
			return nil, &ParamError{err}
		}
		if err := p.Validate(); err != nil {
			return nil, &ParamError{err}
		}
		return h(ctx, p)
	}}
}

// decodeStrict разбирает raw в v; пустое значение и null трактуются как {}.
func decodeStrict(raw json.RawMessage, v any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		raw = []byte("{}")
	}
	if raw[0] != '{' {
		return errors.New("params должен быть JSON-объектом")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("лишние данные после params")
	}
	return nil
}

// Registry — фиксированный набор операций. После NewRegistry не меняется.
type Registry struct {
	ops map[string]Op
}

// NewRegistry собирает реестр; дубликаты имён — ошибка программиста.
func NewRegistry(ops ...Op) *Registry {
	r := &Registry{ops: make(map[string]Op, len(ops))}
	for _, op := range ops {
		if op.call == nil {
			panic("helper: операция не создана через Define: " + op.Name)
		}
		if _, dup := r.ops[op.Name]; dup {
			panic("helper: повтор операции " + op.Name)
		}
		r.ops[op.Name] = op
	}
	return r
}

// Names — имена операций по алфавиту.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.ops))
	for n := range r.ops {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// IsMutating — операция name изменяет систему.
func (r *Registry) IsMutating(name string) bool { return r.ops[name].Mutating }

// Execute выполняет запрос и формирует ответ (без ID/версии).
func (r *Registry) Execute(ctx context.Context, name string, params json.RawMessage) Response {
	op, ok := r.ops[name]
	if !ok {
		return errResponse(CodeUnknownOp, fmt.Sprintf("операция %q не поддерживается", name))
	}
	ctx, cancel := context.WithTimeout(ctx, op.Timeout)
	defer cancel()
	res, err := op.call(ctx, params)
	if err != nil {
		var pe *ParamError
		switch {
		case errors.As(err, &pe):
			return errResponse(CodeInvalidParams, pe.Error())
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrTimeout):
			return errResponse(CodeTimeout, err.Error())
		case errors.Is(err, home.ErrRefused):
			return errResponse(CodeRefused, err.Error())
		case errors.Is(err, nas.ErrNotFound):
			return errResponse(CodeNotFound, err.Error())
		case errors.Is(err, nas.ErrConflict):
			return errResponse(CodeConflict, err.Error())
		case errors.Is(err, nas.ErrNoSpace):
			return errResponse(CodeNoSpace, err.Error())
		case errors.Is(err, nas.ErrInvalid):
			return errResponse(CodeInvalidParams, err.Error())
		default:
			return errResponse(CodeFailed, err.Error())
		}
	}
	var file *os.File
	if wf, ok := res.(WithFile); ok {
		res, file = wf.Result, wf.File
	}
	data, err := json.Marshal(res)
	if err != nil {
		if file != nil {
			file.Close()
		}
		return errResponse(CodeInternal, "результат не сериализуется: "+err.Error())
	}
	return Response{OK: true, Result: data, file: file}
}

func errResponse(code, msg string) Response {
	return Response{OK: false, Error: &ErrorBody{Code: code, Message: msg}}
}

// ExecuteFile — Execute с файлом результата (WithFile) отдельно; для
// выполнения операций в процессе (files.ExecHelper в тестах).
func (r *Registry) ExecuteFile(ctx context.Context, name string, params json.RawMessage) (Response, *os.File) {
	resp := r.Execute(ctx, name, params)
	f := resp.file
	resp.file = nil
	return resp, f
}
