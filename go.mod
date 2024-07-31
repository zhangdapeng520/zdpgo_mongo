module github.com/zhangdapeng520/zdpgo_mongo

go 1.22

require (
	github.com/golang/snappy v0.0.4
	github.com/klauspost/compress v1.13.6
	github.com/montanaflynn/stats v0.7.1
	github.com/xdg-go/scram v1.1.2
	github.com/xdg-go/stringprep v1.0.4
	github.com/youmark/pkcs8 v0.0.0-20181117223130-1be2e3e5546d
	golang.org/x/crypto v0.22.0
	golang.org/x/sync v0.7.0
)

require (
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	golang.org/x/text v0.14.0 // indirect
)

replace golang.org/x/net/http2 => golang.org/x/net/http2 v0.23.0 // GODRIVER-3225
