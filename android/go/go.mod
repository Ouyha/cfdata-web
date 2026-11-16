module cfdatamobile

go 1.26.0

require github.com/gorilla/websocket v1.5.3

require (
	golang.org/x/mobile v0.0.0-20261011034528-197770444867 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
)

replace github.com/gorilla/websocket => ../../.gomodcache/github.com/gorilla/websocket@v1.5.3

tool golang.org/x/mobile/cmd/gobind
