BINARY := be-code
VERSION := 0.15.0
LDFLAGS := -s -w -X github.com/brown-enterprises/be-code/cmd.Version=$(VERSION)

.PHONY: build test vet verify clean release vscode visualstudio-test

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

verify: vet build test

clean:
	rm -f $(BINARY) $(BINARY).exe
	rm -rf dist

vscode: ## build and package the VS Code extension into dist/
	mkdir -p dist
	cd vscode && npm install --no-audit --no-fund && npm test && npm run package

visualstudio-test: ## compile the Visual Studio bridge (VSIX layer included) and run its tests; needs the .NET SDK, not part of verify
	cd visualstudio && dotnet build --no-incremental && dotnet test --no-build

release: verify vscode
	mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 .
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 .
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe .
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64 .
