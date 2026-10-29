//go:build linux

package rescue

import "strings"

var currentKeyboard = "DE"

// setKeyboard loads the German ("de") or US console keymap.
func setKeyboard(layout string) {
	if _, err := sh("loadkeys", layout); err == nil {
		currentKeyboard = strings.ToUpper(layout)
	}
}

func keyboardName() string { return currentKeyboard }
