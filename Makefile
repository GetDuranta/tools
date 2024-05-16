
cross-compile:
	mkdir -p bin
	GOOS=darwin GOARCH=amd64 go build -o bin/ddev-darwin-amd64 ddev/main/main.go
	GOOS=darwin GOARCH=arm64 go build -o bin/ddev-darwin-arm64 ddev/main/main.go
	GOOS=linux GOARCH=amd64 go build -o bin/ddev-linux-amd64 ddev/main/main.go
	GOOS=linux GOARCH=arm64 go build -o bin/ddev-linux-arm64 ddev/main/main.go
	GOOS=windows GOARCH=amd64 go build -o bin/ddev-windows-amd64.exe ddev/main/main.go
	GOOS=windows GOARCH=arm64 go build -o bin/ddev-windows-arm64.exe ddev/main/main.go

upload:
	if [ -z "$(GITHUB_SHA)" ]; then echo "GITHUB_SHA env var is not set"; exit 1; fi
	aws s3 cp --acl public-read --content-type application/octet-stream \
		bin/ddev-darwin-amd64 "s3://duranta-tool-artifacts/ddev/$(GITHUB_SHA)/ddev-darwin-amd64"
	aws s3 cp --acl public-read --content-type application/octet-stream \
		bin/ddev-darwin-arm64 "s3://duranta-tool-artifacts/ddev/$(GITHUB_SHA)/ddev-darwin-arm64"
	aws s3 cp --acl public-read --content-type application/octet-stream \
		bin/ddev-linux-amd64 "s3://duranta-tool-artifacts/ddev/$(GITHUB_SHA)/ddev-linux-amd64"
	aws s3 cp --acl public-read --content-type application/octet-stream \
		bin/ddev-linux-arm64 "s3://duranta-tool-artifacts/ddev/$(GITHUB_SHA)/ddev-linux-arm64"
	aws s3 cp --acl public-read --content-type application/octet-stream \
		bin/ddev-windows-amd64.exe "s3://duranta-tool-artifacts/ddev/$(GITHUB_SHA)/ddev-windows-amd64.exe"
	aws s3 cp --acl public-read --content-type application/octet-stream \
		bin/ddev-windows-arm64.exe "s3://duranta-tool-artifacts/ddev/$(GITHUB_SHA)/ddev-windows-arm64.exe"

update-rds-ca-certs:
	curl -o ddev/cmds/rds-ca-certs.pem https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem
