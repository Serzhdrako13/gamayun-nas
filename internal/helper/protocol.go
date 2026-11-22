// Package helper — привилегированный исполнитель gamayun-helper и клиент к нему.
//
// gamayun-helper работает от root и выполняет только операции из
// фиксированного реестра (Registry), собранного в коде. Произвольных
// команд нет: каждая операция — функция Go со строго проверяемыми
// параметрами. Доступ — через Unix-сокет; соединение принимается, только
// если uid процесса-клиента (SO_PEERCRED) совпадает с uid gamayund.
//
// Протокол — JSON построчно (NDJSON): один запрос — одна строка, ответ —
// одна строка. Компактный JSON не содержит перевода строки (в строках он
// экранируется), поэтому разделитель однозначен; длина строки ограничена
// MaxRequestBytes. Длинный префикс не нужен: двоичных данных нет, а
// построчный формат проверяется вручную через socat/nc -U.
//
// Запрос:  {"v":1,"id":"…","op":"system.info","params":{}}
// Ответ:   {"v":1,"id":"…","ok":true,"result":{…}}
//
//	{"v":1,"id":"…","ok":false,"error":{"code":"invalid_params","message":"…"}}
package helper

import (
	"encoding/json"
	"os"
)

// ProtocolVersion — версия протокола; запрос с другой версией отклоняется.
const ProtocolVersion = 1

// MaxRequestBytes — предельная длина строки запроса.
const MaxRequestBytes = 64 << 10

// MaxResponseBytes — предельная длина строки ответа на стороне клиента.
const MaxResponseBytes = 16 << 20

// Request — запрос к helper.
type Request struct {
	V      int             `json:"v"`
	ID     string          `json:"id,omitempty"` // эхо в ответе, до 64 символов
	Op     string          `json:"op"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response — ответ helper.
type Response struct {
	V      int             `json:"v"`
	ID     string          `json:"id,omitempty"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorBody      `json:"error,omitempty"`

	file *os.File // файл для передачи SCM_RIGHTS (WithFile), не сериализуется
}

// ErrorBody — ошибка операции.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Коды ошибок протокола.
const (
	CodeBadRequest    = "bad_request"    // не JSON, лишние поля конверта, неверная версия
	CodeTooLarge      = "too_large"      // строка длиннее MaxRequestBytes
	CodeUnknownOp     = "unknown_op"     // операции нет в реестре
	CodeInvalidParams = "invalid_params" // параметры не прошли проверку
	CodeForbidden     = "forbidden"      // uid клиента не разрешён
	CodeTimeout       = "timeout"        // операция превысила лимит времени
	CodeFailed        = "failed"         // операция завершилась ошибкой
	CodeRefused       = "refused"        // отказ проверок безопасности до записи
	CodeNotFound      = "not_found"      // объекта нет (nas.ErrNotFound)
	CodeConflict      = "conflict"       // конфликт состояния: имя занято, каталог не пуст (nas.ErrConflict)
	CodeNoSpace       = "no_space"       // недостаточно места (nas.ErrNoSpace)
	CodeInternal      = "internal"
)
