//go:build vivahead

// This file only exists in builds tagged vivahead: with VIVA_HEADLESS=1 the
// window is not opened and viva reads its line protocol (click, type, shot,
// layout …) from stdin, for UI checks and screenshots. Release builds never
// contain it.
package main

import _ "github.com/vivaui/viva/headless/auto"
