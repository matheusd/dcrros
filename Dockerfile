# This is the recommended Dockerfile for production deployments of dcrros+dcrd
# as of the commit this is on.
#
# It builds a docker image that includes dcrd, dcrctl and dcrros and runs
# dcrros with dcrd in embedded mode (that is, dcrros controls the dcrd process).
#
# See the docs for alternative ways of running this.

# Stage 1: Build the bins in a golang image.
FROM golang:1.14-buster AS builder

# Build dcrd and include dcrctl as well.
#
# TODO: Switch to the tagged 1.6.0 once that is released.
RUN git clone https://github.com/decred/dcrd
RUN (cd dcrd && git checkout cee62c352c75b84f303da419c6b8b56d62c2949e)
RUN (cd dcrd && go install .)
RUN git clone https://github.com/decred/dcrctl
RUN (cd dcrctl && git checkout 44e17b578ad6a7d3769be4574196867b4c34f4e8)
RUN (cd dcrctl && go install .)
RUN git clone https://github.com/decred/dcrros
RUN (cd dcrros && git checkout aa76a84a19fcda926bd30ea6eaa29815d9b907dd)
RUN (cd dcrros && go install .)

# Stage 2: Build the final image starting from a cleaner base.
FROM debian:buster

# Install ca-certificates so dcrd can reach the network seeders.
RUN apt-get update
RUN apt-get install -y --no-install-recommends ca-certificates
RUN update-ca-certificates

# Copy the previously built bins.
COPY --from=builder /go/bin/dcrd bin/dcrd
COPY --from=builder /go/bin/dcrctl bin/dcrctl
COPY --from=builder /go/bin/dcrros bin/dcrros

# According to Rosetta's documentation, all data should be in /data.
WORKDIR /data

# Expose dcrros and dcrd ports for mainnet, testnet and simnet. Each line is:
# 	dcrros    dcrd-p2p  dcrd-rpc
EXPOSE  9128/tcp  9108/tcp  9109/tcp 
EXPOSE 19128/tcp 19108/tcp 19109/tcp 
EXPOSE 29128/tcp 19556/tcp 18555/tcp

# The main executable for this is dcrros, running dcrd in "embedded" mode:
# dcrros runs dcrd and controls the lifetime of its process. Once dcrros is
# commanded to stop, it stops the underlying dcrd.
ENTRYPOINT ["dcrros", \
	"--appdata=/data/dcrros", \
	"--rundcrd=dcrd", \
	"--dcrdextraarg=\"--appdata=/data/dcrd\"", \
	"--dcrdcertpath=/data/dcrd/rpc.cert" ]
