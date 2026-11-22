package confbackup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gamayun-nas/internal/events"

	"golang.org/x/crypto/ssh"
)

// Типы событий (без секретов: только пул, имя файла, причина).
const (
	EventSaved  = "config.backup.saved"
	EventFailed = "config.backup.failed"
)

// AutoInterval — проверка изменений настроек; DailyInterval — копия не
// реже раза в сутки, даже без изменений.
const (
	AutoInterval  = time.Hour
	DailyInterval = 24 * time.Hour
)

const keyLast = "config.backup.last"

// HelperClient — вызов операции gamayun-helper.
type HelperClient interface {
	Call(ctx context.Context, op string, params, result any) error
}

// Service — копии настроек в gamayund: выгрузка, автокопии в пулы, USB.
type Service struct {
	DB     *sql.DB
	Helper HelperClient
	Events *events.Log
	Logger *slog.Logger
	// Identity — часовой пояс и NTP через helper; nil или ошибка — без них.
	Identity func(ctx context.Context) (*Identity, error)
	Now      func() time.Time
	// SFTPKeyFile — закрытый ключ NAS для SFTP (создаётся при первом обращении).
	SFTPKeyFile string
	// Upload — выгрузка по SFTP; nil — UploadSFTP (подменяется в тестах).
	Upload func(ctx context.Context, c SFTPConfig, signer ssh.Signer, name string, data []byte, keep int) (SaveResult, error)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Export — файл копии и его имя.
func (s *Service) Export(ctx context.Context) ([]byte, string, Backup, error) {
	var id *Identity
	if s.Identity != nil {
		if v, err := s.Identity(ctx); err == nil {
			id = v
		} else if s.Logger != nil {
			s.Logger.Warn("копия настроек: часовой пояс не прочитан", "err", err)
		}
	}
	b, _, err := Collect(ctx, s.DB, id, s.now())
	if err != nil {
		return nil, "", b, err
	}
	data, err := Encode(b)
	if err != nil {
		return nil, "", b, err
	}
	return data, FileName(b.CreatedAt, b.Hostname), b, nil
}

// PoolSave — итог сохранения в один пул.
type PoolSave struct {
	Dest    string   `json:"dest"` // pool | sftp
	Pool    string   `json:"pool,omitempty"`
	Name    string   `json:"name,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type last struct {
	SHA256 string    `json:"sha256"`
	At     time.Time `json:"at"`
}

func (s *Service) poolNames(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT name FROM pools ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ErrNoPools — нет пулов для автокопии.
var ErrNoPools = errors.New("нет пулов: копию в пул сохранить некуда")

// SaveToPools сохраняет копию во все пулы. force=false — только если
// настройки изменились с прошлой копии или прошли сутки.
// saved=false, err=nil — копия не нужна.
func (s *Service) SaveToPools(ctx context.Context, force bool) ([]PoolSave, bool, error) {
	// Автокопия — только после мастера: новая система после
	// переустановки и подхвата пула иначе записала бы на пул свою пустую
	// копию поверх настоящих (самая свежая, а ротация вытесняет старые).
	if !force {
		if v, err := s.kv(ctx, "setup.done"); err != nil || v != "1" {
			return nil, false, nil
		}
	}
	data, name, b, err := s.Export(ctx)
	if err != nil {
		return nil, false, err
	}
	var l last
	if v, err := s.kv(ctx, keyLast); err == nil && v != "" {
		json.Unmarshal([]byte(v), &l)
	}
	if !force && l.SHA256 == b.SHA256 && s.now().Sub(l.At) < DailyInterval {
		return nil, false, nil
	}
	pools, err := s.poolNames(ctx)
	if err != nil {
		return nil, false, err
	}
	sc, err := LoadSFTP(ctx, s.DB)
	if err != nil {
		return nil, false, err
	}
	if len(pools) == 0 && !sc.Enabled {
		return nil, false, ErrNoPools
	}
	out := []PoolSave{}
	ok := 0
	for _, p := range pools {
		var r SaveResult
		ps := PoolSave{Dest: "pool", Pool: p, Name: name}
		if err := s.Helper.Call(ctx, OpSave, SaveParams{Pool: p, Name: name, Data: data}, &r); err != nil {
			ps.Error = err.Error()
			s.emit(ctx, EventFailed, events.SevWarning, p, map[string]any{"pool": p, "error": err.Error()})
		} else {
			ps.Removed = r.Removed
			ok++
			s.emit(ctx, EventSaved, events.SevInfo, p, map[string]any{"pool": p, "file": name, "removed": len(r.Removed)})
		}
		out = append(out, ps)
	}
	if sc.Enabled {
		ps := PoolSave{Dest: "sftp", Name: name}
		if r, err := s.uploadSFTP(ctx, sc, name, data); err != nil {
			ps.Error = err.Error()
			s.emit(ctx, EventFailed, events.SevWarning, "", map[string]any{"sftp": sc.Host, "error": err.Error()})
		} else {
			ps.Removed = r.Removed
			ok++
			s.emit(ctx, EventSaved, events.SevInfo, "", map[string]any{"sftp": sc.Host, "file": name, "removed": len(r.Removed)})
		}
		out = append(out, ps)
	}
	if ok > 0 {
		v, _ := json.Marshal(last{SHA256: b.SHA256, At: s.now().UTC()})
		if err := s.setKV(ctx, keyLast, string(v)); err != nil {
			return out, true, err
		}
	}
	if ok == 0 {
		return out, false, fmt.Errorf("копия настроек не сохранена ни в одно место")
	}
	return out, true, nil
}

func (s *Service) uploadSFTP(ctx context.Context, c SFTPConfig, name string, data []byte) (SaveResult, error) {
	signer, _, err := s.key()
	if err != nil {
		return SaveResult{}, err
	}
	up := s.Upload
	if up == nil {
		up = UploadSFTP
	}
	return up(ctx, c, signer, name, data, Keep)
}

func (s *Service) key() (ssh.Signer, string, error) {
	if s.SFTPKeyFile == "" {
		return nil, "", errors.New("ключ SFTP не настроен")
	}
	return EnsureKey(s.SFTPKeyFile)
}

// SFTPInfo — настройки назначения и открытый ключ NAS (для authorized_keys).
type SFTPInfo struct {
	Config          SFTPConfig `json:"config"`
	HostFingerprint string     `json:"host_fingerprint,omitempty"`
	PublicKey       string     `json:"public_key"`
	KeyFingerprint  string     `json:"key_fingerprint"`
}

// SFTP — текущее состояние.
func (s *Service) SFTP(ctx context.Context) (SFTPInfo, error) {
	c, err := LoadSFTP(ctx, s.DB)
	if err != nil {
		return SFTPInfo{}, err
	}
	_, pub, err := s.key()
	if err != nil {
		return SFTPInfo{}, err
	}
	return SFTPInfo{Config: c, HostFingerprint: Fingerprint(c.HostKey), PublicKey: pub, KeyFingerprint: Fingerprint(pub)}, nil
}

// SetSFTP сохраняет назначение.
func (s *Service) SetSFTP(ctx context.Context, c SFTPConfig) (SFTPInfo, error) {
	if err := SaveSFTP(ctx, s.DB, c); err != nil {
		return SFTPInfo{}, err
	}
	return s.SFTP(ctx)
}

// UploadNow — свежая копия на SFTP сейчас (и проверка назначения).
func (s *Service) UploadNow(ctx context.Context) (SaveResult, error) {
	c, err := LoadSFTP(ctx, s.DB)
	if err != nil {
		return SaveResult{}, err
	}
	if c.Host == "" {
		return SaveResult{}, fmt.Errorf("%w: назначение не задано", ErrSFTP)
	}
	data, name, _, err := s.Export(ctx)
	if err != nil {
		return SaveResult{}, err
	}
	r, err := s.uploadSFTP(ctx, c, name, data)
	if err != nil {
		s.emit(ctx, EventFailed, events.SevWarning, "", map[string]any{"sftp": c.Host, "error": err.Error()})
		return r, err
	}
	s.emit(ctx, EventSaved, events.SevInfo, "", map[string]any{"sftp": c.Host, "file": name})
	return r, nil
}

// PoolFiles — копии в пуле.
type PoolFiles struct {
	Pool  string     `json:"pool"`
	Files []FileInfo `json:"files"`
	Error string     `json:"error,omitempty"`
}

// List — копии во всех пулах.
func (s *Service) List(ctx context.Context) ([]PoolFiles, error) {
	pools, err := s.poolNames(ctx)
	if err != nil {
		return nil, err
	}
	out := []PoolFiles{}
	for _, p := range pools {
		pf := PoolFiles{Pool: p, Files: []FileInfo{}}
		var files []FileInfo
		if err := s.Helper.Call(ctx, OpList, PoolParams{Pool: p}, &files); err != nil {
			pf.Error = err.Error()
		} else if files != nil {
			pf.Files = files
		}
		out = append(out, pf)
	}
	return out, nil
}

// Read — файл копии из пула.
func (s *Service) Read(ctx context.Context, pool, name string) ([]byte, error) {
	if err := (ReadParams{Pool: pool, Name: name}).Validate(); err != nil {
		return nil, err
	}
	var r ReadResult
	err := s.Helper.Call(ctx, OpRead, ReadParams{Pool: pool, Name: name}, &r)
	return r.Data, err
}

// USBDevices — носители для выгрузки.
func (s *Service) USBDevices(ctx context.Context) ([]USBDevice, error) {
	var out []USBDevice
	if err := s.Helper.Call(ctx, OpUSBList, nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []USBDevice{}
	}
	return out, nil
}

// SaveUSB выгружает свежую копию на USB-носитель.
func (s *Service) SaveUSB(ctx context.Context, device string) (SaveResult, error) {
	data, name, _, err := s.Export(ctx)
	if err != nil {
		return SaveResult{}, err
	}
	var r SaveResult
	if err := s.Helper.Call(ctx, OpUSBSave, USBSaveParams{Device: device, Name: name, Data: data}, &r); err != nil {
		s.emit(ctx, EventFailed, events.SevWarning, "", map[string]any{"usb": device, "error": err.Error()})
		return r, err
	}
	s.emit(ctx, EventSaved, events.SevInfo, "", map[string]any{"usb": device, "file": name})
	return r, nil
}

// Run — автокопии: при старте и каждый AutoInterval.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(AutoInterval)
	defer t.Stop()
	for {
		if _, saved, err := s.SaveToPools(ctx, false); err != nil && !errors.Is(err, ErrNoPools) && ctx.Err() == nil {
			if s.Logger != nil {
				s.Logger.Warn("автокопия настроек", "err", err)
			}
		} else if saved && s.Logger != nil {
			s.Logger.Info("автокопия настроек сохранена")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) emit(ctx context.Context, typ, sev, pool string, data map[string]any) {
	if s.Events == nil {
		return
	}
	b, _ := json.Marshal(data)
	subj := ""
	if pool != "" {
		subj = "pool:" + pool
	}
	s.Events.Emit(ctx, events.Event{Type: typ, Severity: sev, Subject: subj, Data: b}, true)
}

func (s *Service) kv(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Service) setKV(ctx context.Context, key, val string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, val)
	return err
}

// DecodeBytes — Decode для среза.
func DecodeBytes(b []byte, schemaVersion int) (Backup, Data, error) {
	return Decode(bytes.NewReader(b), schemaVersion)
}
