// Package webui 把控制台单页编入二进制。
package webui

import _ "embed"

//go:embed index.html
var indexHTML []byte

// Index 返回控制台页面。
func Index() []byte { return indexHTML }
