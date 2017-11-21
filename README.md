# btcexport

TODO: Explain what this is.

# Usage

```
Usage:
  btcexport [OPTIONS]

Application Options:
  -b, --datadir=      Location of the btcd data directory (default: /home/jimpo/.btcd/data)
      --dbtype=       Database backend to use for the Block Chain (default: ffldb)
      --testnet       Use the test network
      --regtest       Use the regression test network
      --simnet        Use the simulation test network
      --output=       Directory to write output files to
      --s3-bucket=    S3 bucket to write output files to
      --s3-prefix=    Key prefix of S3 objects to upload
      --start-height= Optional beginning height of export range (default=0)
      --end-height=   Ending height of of export range (default=tip-6)
  -p, --progress=     Show a progress message each time this number of seconds have passed -- Use 0 to disable progress announcements (default: 10)

Help Options:
  -h, --help          Show this help message
```

# Installing

```bash
$ go get -u github.com/coinbase/btcexport
$ cd $GOPATH/src/github.com/coinbase/btcexport
$ dep ensure
$ go install ./cmd/...
```