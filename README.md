# Build

```
go generate && go build
```

The generate step will download and build statically the libsodium.

The generate step rely on some tools to build it:

- autotools
- automake
- make
- gcc
- git

Is possible to link the go-lederlog-tool to a libsodium already installed on the system, to do so change the CGO comments to provide
the correct CFLAGS and LDFLAGS to CGO and skip the generate step.
