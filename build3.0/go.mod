module cfdata3

go 1.24

require github.com/gorilla/websocket v1.5.3

// Reuse the dependency cache prepared next to the project root.
replace github.com/gorilla/websocket => ../.gomodcache/github.com/gorilla/websocket@v1.5.3
