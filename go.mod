module github.com/barqdb/barq-server

go 1.24.0

toolchain go1.26.5

require (
	github.com/BarqDB/barq-go v0.0.0-20260713030141-2a3691750fc0
	github.com/fastschema/qjs v0.0.6
	github.com/swaggest/swgui v1.8.8
)

replace github.com/BarqDB/barq-go => ../client/barq-go

require (
	github.com/shurcooL/httpgzip v0.0.0-20190720172056-320755c1c1b0 // indirect
	github.com/tetratelabs/wazero v1.11.0 // indirect
	golang.org/x/net v0.8.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.8.0 // indirect
)
