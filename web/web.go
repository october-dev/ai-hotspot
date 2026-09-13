// Package web embeds the self-contained admin dashboard.
package web

import _ "embed"

// DashboardHTML is the single-file operator dashboard served at "/".
//
//go:embed dashboard.html
var DashboardHTML string
