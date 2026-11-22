package apps

// POSIX ACL общих папок для учёток приложений — кодирование
// xattr system.posix_acl_access / system.posix_acl_default (формат ядра,
// версия 2) и правка записей. Чистые функции: тестируются на любой ОС.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Теги записей ACL (linux/posix_acl.h).
const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
	aclUndef    = 0xFFFFFFFF
	aclVersion  = 2
	permR       = 4
	permW       = 2
	permX       = 1
)

// aclEntry — запись ACL.
type aclEntry struct {
	Tag  uint16
	Perm uint16
	ID   uint32
}

// ACL — набор записей.
type ACL []aclEntry

func parseACL(b []byte) (ACL, error) {
	if len(b) < 4 || (len(b)-4)%8 != 0 {
		return nil, errors.New("ACL: неверная длина")
	}
	if v := binary.LittleEndian.Uint32(b); v != aclVersion {
		return nil, fmt.Errorf("ACL: версия %d", v)
	}
	var a ACL
	for i := 4; i < len(b); i += 8 {
		a = append(a, aclEntry{Tag: binary.LittleEndian.Uint16(b[i:]), Perm: binary.LittleEndian.Uint16(b[i+2:]),
			ID: binary.LittleEndian.Uint32(b[i+4:])})
	}
	return a, nil
}

func (a ACL) encode() []byte {
	a.sort()
	b := make([]byte, 4+8*len(a))
	binary.LittleEndian.PutUint32(b, aclVersion)
	for i, e := range a {
		o := 4 + 8*i
		binary.LittleEndian.PutUint16(b[o:], e.Tag)
		binary.LittleEndian.PutUint16(b[o+2:], e.Perm)
		binary.LittleEndian.PutUint32(b[o+4:], e.ID)
	}
	return b
}

// sort — порядок, который требует ядро: по тегу, внутри — по id.
func (a ACL) sort() {
	sort.Slice(a, func(i, j int) bool {
		if a[i].Tag != a[j].Tag {
			return a[i].Tag < a[j].Tag
		}
		return a[i].ID < a[j].ID
	})
}

// aclFromMode — минимальный ACL по правам файла.
func aclFromMode(mode uint32) ACL {
	return ACL{
		{Tag: aclUserObj, Perm: uint16(mode>>6) & 7, ID: aclUndef},
		{Tag: aclGroupObj, Perm: uint16(mode>>3) & 7, ID: aclUndef},
		{Tag: aclOther, Perm: uint16(mode) & 7, ID: aclUndef},
	}
}

func (a ACL) find(tag uint16, id uint32) int {
	for i, e := range a {
		if e.Tag == tag && (e.ID == id || tag != aclUser && tag != aclGroup) {
			return i
		}
	}
	return -1
}

// set — записать (или заменить) запись.
func (a ACL) set(tag uint16, id uint32, perm uint16) ACL {
	if tag != aclUser && tag != aclGroup {
		id = aclUndef
	}
	if i := a.find(tag, id); i >= 0 {
		a[i].Perm = perm
		return a
	}
	return append(a, aclEntry{Tag: tag, Perm: perm, ID: id})
}

func (a ACL) remove(tag uint16, id uint32) ACL {
	out := a[:0]
	for _, e := range a {
		if e.Tag == tag && e.ID == id {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (a ACL) perm(tag uint16, id uint32) (uint16, bool) {
	if i := a.find(tag, id); i >= 0 {
		return a[i].Perm, true
	}
	return 0, false
}

// named — есть именованные записи (user:N / group:N).
func (a ACL) named() bool {
	for _, e := range a {
		if e.Tag == aclUser || e.Tag == aclGroup {
			return true
		}
	}
	return false
}

// recalcMask — mask = объединение прав группового класса (как setfacl без
// -n); без именованных записей mask не нужна (минимальный ACL).
func (a ACL) recalcMask() ACL {
	if !a.named() {
		return a.remove(aclMask, aclUndef)
	}
	var m uint16
	for _, e := range a {
		if e.Tag == aclUser || e.Tag == aclGroup || e.Tag == aclGroupObj {
			m |= e.Perm
		}
	}
	return a.set(aclMask, aclUndef, m)
}

// minimal — только записи владельца, группы и остальных.
func (a ACL) minimal() bool {
	for _, e := range a {
		if e.Tag != aclUserObj && e.Tag != aclGroupObj && e.Tag != aclOther {
			return false
		}
	}
	return true
}

// clone — копия (ACL правится на месте).
func (a ACL) clone() ACL { return append(ACL{}, a...) }

// grantPerm — права выдачи: r-X / rwX (X — только каталогам и исполняемым).
func grantPerm(mode string, isDir, exec bool) uint16 {
	p := uint16(permR)
	if mode == GrantModeRW {
		p |= permW
	}
	if isDir || exec {
		p |= permX
	}
	return p
}

// Режимы операции apps.acl.start.
const (
	GrantModeRO     = "ro"
	GrantModeRW     = "rw"
	GrantModeRevoke = "revoke"
)

// aclChange — правка ACL одного объекта.
type aclChange struct {
	Mode string // ro | rw | revoke
	UID  uint32
	// GID — группа файлов NAS (gamayun-users) для d:g:<gid>:rwX при rw.
	GID uint32
	// DropGroupDefault — при отзыве снять и d:g:<gid> (других выдач на запись нет).
	DropGroupDefault bool
}

// applyAccess — новый access ACL объекта. cur — текущий (nil — из mode).
// changed=false — ACL уже такой.
func (c aclChange) applyAccess(cur ACL, mode uint32, isDir bool) (ACL, bool) {
	if cur == nil {
		cur = aclFromMode(mode)
	}
	before := string(cur.clone().encode())
	a := cur.clone()
	if c.Mode == GrantModeRevoke {
		a = a.remove(aclUser, c.UID)
		if c.DropGroupDefault {
			// унаследованная от d:g запись группы NAS у файлов, созданных после выдачи
			a = a.remove(aclGroup, c.GID)
		}
	} else {
		a = a.set(aclUser, c.UID, grantPerm(c.Mode, isDir, mode&0o111 != 0))
	}
	a = a.recalcMask()
	return a, string(a.clone().encode()) != before
}

// applyDefault — новый default ACL каталога (nil — снять default ACL).
// base — access ACL каталога (для базовых записей нового default ACL).
func (c aclChange) applyDefault(cur, base ACL) (ACL, bool) {
	before := ""
	if cur != nil {
		before = string(cur.clone().encode())
	}
	var a ACL
	if cur != nil {
		a = cur.clone()
	} else {
		if c.Mode == GrantModeRevoke {
			return nil, false
		}
		for _, e := range base {
			if e.Tag == aclUserObj || e.Tag == aclGroupObj || e.Tag == aclOther {
				a = append(a, e)
			}
		}
	}
	switch c.Mode {
	case GrantModeRevoke:
		a = a.remove(aclUser, c.UID)
		if c.DropGroupDefault {
			a = a.remove(aclGroup, c.GID)
		}
	default:
		a = a.set(aclUser, c.UID, grantPerm(c.Mode, true, false))
		if c.Mode == GrantModeRW {
			a = a.set(aclGroup, c.GID, permR|permW|permX)
		}
	}
	a = a.recalcMask()
	if a.minimal() {
		// Только базовые записи — default ACL не нужен (исходное состояние).
		return nil, before != ""
	}
	return a, string(a.clone().encode()) != before
}
