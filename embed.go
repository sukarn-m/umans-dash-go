package main

import _ "embed"

//go:embed dashboard.html
var dashboardHTML []byte

//go:embed dashboard.js
var dashboardJS []byte
