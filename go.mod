module github.com/zrep/zrep

go 1.26.2

require github.com/oarkflow/bcl v0.0.19

require (
	github.com/dlclark/regexp2 v1.12.0
	github.com/oarkflow/xql v0.0.1
)

require (
	github.com/oarkflow/spb v0.0.2 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/oarkflow/xql => ../xql
