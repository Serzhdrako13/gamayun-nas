// Команда gamayun-disks печатает JSON-снимок дисков с ролями (root, boot, efi,
// swap, mounted, stack_member) для ручной проверки. Только чтение sysfs/proc:
// систему не меняет. Код выхода 0 — снимок получен (complete может быть false),
// 1 — источник недоступен.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"gamayun-nas/internal/disks"
)

func main() {
	snap, err := disks.Observe(disks.DefaultPaths())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gamayun-disks:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(snap); err != nil {
		fmt.Fprintln(os.Stderr, "gamayun-disks:", err)
		os.Exit(1)
	}
}
