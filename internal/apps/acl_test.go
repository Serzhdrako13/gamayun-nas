package apps

import (
	"bytes"
	"testing"
)

func TestACLEncodeRoundTrip(t *testing.T) {
	a := ACL{{aclOther, 0, aclUndef}, {aclUser, 7, 59001}, {aclUserObj, 7, aclUndef}, {aclGroupObj, 5, aclUndef}, {aclMask, 7, aclUndef}}
	b := a.encode()
	p, err := parseACL(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.encode(), b) || p[0].Tag != aclUserObj || p[1].Tag != aclUser || p[4].Tag != aclOther {
		t.Fatalf("порядок/кодирование: %+v", p)
	}
	if _, err := parseACL([]byte{1, 0, 0, 0}); err == nil {
		t.Fatal("версия 1 принята")
	}
	if _, err := parseACL([]byte{2, 0, 0, 0, 1}); err == nil {
		t.Fatal("неверная длина принята")
	}
}

func TestACLGrantRevoke(t *testing.T) {
	const uid, gid = 59001, 1001
	// файл 0660 (как в общих папках), выдача ro → u:59001:r--, mask rw-
	rw := aclChange{Mode: GrantModeRW, UID: uid, GID: gid}
	ro := aclChange{Mode: GrantModeRO, UID: uid, GID: gid}
	rv := aclChange{Mode: GrantModeRevoke, UID: uid, GID: gid, DropGroupDefault: true}
	a, ch := ro.applyAccess(nil, 0o660, false)
	if !ch {
		t.Fatal("не изменено")
	}
	if p, _ := a.perm(aclUser, uid); p != permR {
		t.Fatalf("ro файл: %o", p)
	}
	if m, _ := a.perm(aclMask, aclUndef); m != permR|permW {
		t.Fatalf("mask: %o", m)
	}
	// повтор — без изменений
	if _, ch := ro.applyAccess(a, 0o660, false); ch {
		t.Fatal("повтор изменил ACL")
	}
	// rw каталог: rwx; исполняемый файл — X
	d, _ := rw.applyAccess(nil, 0o2770, true)
	if p, _ := d.perm(aclUser, uid); p != 7 {
		t.Fatalf("rw каталог: %o", p)
	}
	x, _ := ro.applyAccess(nil, 0o750, false)
	if p, _ := x.perm(aclUser, uid); p != permR|permX {
		t.Fatalf("исполняемый: %o", p)
	}
	// отзыв — минимальный ACL, mask снят, права группы прежние
	r, ch := rv.applyAccess(a, 0o660, false)
	if !ch || !r.minimal() {
		t.Fatalf("отзыв: %+v", r)
	}
	if g, _ := r.perm(aclGroupObj, aclUndef); g != permR|permW {
		t.Fatalf("группа после отзыва: %o", g)
	}
	// default: rw → d:u, d:g, mask; отзыв с DropGroupDefault → снят целиком
	def, ch := rw.applyDefault(nil, d)
	if !ch {
		t.Fatal("default не создан")
	}
	if p, _ := def.perm(aclGroup, gid); p != 7 {
		t.Fatalf("d:g: %o", p)
	}
	if p, _ := def.perm(aclUser, uid); p != 7 {
		t.Fatalf("d:u: %o", p)
	}
	if p, _ := def.perm(aclGroupObj, aclUndef); p != 7 {
		t.Fatalf("d:g:: из прав каталога: %o", p)
	}
	nd, ch := rv.applyDefault(def, d)
	if !ch || nd != nil {
		t.Fatalf("отзыв default: %+v", nd)
	}
	// отзыв без DropGroupDefault — d:g остаётся
	keep := aclChange{Mode: GrantModeRevoke, UID: uid, GID: gid}
	if k, _ := keep.applyDefault(def, d); k == nil || k.find(aclGroup, gid) < 0 || k.find(aclUser, uid) >= 0 {
		t.Fatalf("отзыв с сохранением группы: %+v", k)
	}
	// отзыв там, где выдачи не было, — без изменений
	if _, ch := rv.applyDefault(nil, d); ch {
		t.Fatal("отзыв без default ACL изменил")
	}
	// ro default: только d:u:r-x, без группы
	rd, _ := ro.applyDefault(nil, d)
	if p, _ := rd.perm(aclUser, uid); p != permR|permX || rd.find(aclGroup, gid) >= 0 {
		t.Fatalf("ro default: %+v", rd)
	}
}

func TestAppUserName(t *testing.T) {
	if AppUser("web") != "app-web" {
		t.Fatal(AppUser("web"))
	}
	long := "abcdefghijklmnopqrstuvwxyz012345"
	if n := AppUser(long); len(n) > 32 || n == AppUser(long[:31]+"6") {
		t.Fatalf("длинное имя: %s", n)
	}
}
