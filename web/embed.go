// Package web holds the embedded dashboard files.
package web

import "embed"

// UPlotVersion is the vendored uPlot release. index.html references the
// assets with ?v=<version> so they can be cached long-term.
const UPlotVersion = "1.6.32"

//go:embed index.html login.html uplot.min.js uplot.min.css
var Files embed.FS
