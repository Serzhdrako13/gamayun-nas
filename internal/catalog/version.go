package catalog

import (
	"strconv"
	"strings"
)

// CompareVersion сравнивает версии вида 1.2.3[-pre] и версии пакета
// gamayun-nas (0.1.0~alpha.160+g62ea9d2.dirty): '~' и '-' — предрелиз (меньше
// релиза), '+…' — сборка, не влияет. Возвращает -1, 0, 1.
func CompareVersion(a, b string) int {
	ab, ap := splitVer(a)
	bb, bp := splitVer(b)
	if c := cmpDotted(ab, bb); c != 0 {
		return c
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	return cmpDotted(ap, bp)
}

func splitVer(v string) (base, pre string) {
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	if i := strings.IndexAny(v, "~-"); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

// cmpDotted — по частям через '.': числа численно, иначе строкой.
func cmpDotted(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xe := strconv.Atoi(x)
		yn, ye := strconv.Atoi(y)
		switch {
		case xe == nil && ye == nil:
			if xn != yn {
				return sign(xn - yn)
			}
		case x == "" || y == "":
			if x != y {
				if x == "" {
					return -1
				}
				return 1
			}
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return 0
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}
