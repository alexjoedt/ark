module github.com/alexjoedt/ark

go 1.26

require (
	github.com/kevinburke/ssh_config v1.6.0
	github.com/klauspost/compress v1.20.2
	github.com/pkg/sftp v1.13.12
	golang.org/x/crypto v0.54.0
	golang.org/x/sys v0.47.0
)

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
)

// Published before the API and the on-disk format had settled; v0.x is the
// development line and v1.0.0 and v1.0.1 must not be used.
retract [v1.0.0, v1.0.1]
