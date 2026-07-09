package proxy

import _ "embed"

//go:embed interface/dashboard.html
var DashboardHTML []byte

//go:embed interface/dashboard.js
var DashboardJS []byte
