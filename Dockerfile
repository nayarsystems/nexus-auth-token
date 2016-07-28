FROM alpine

ENV GOPATH /go/
ADD . /go/src/github.com/nayarsystems/nexus-auth-token

RUN apk update &&\
	apk add go git mercurial &&\
	cd /go/src/github.com/nayarsystems/nexus-auth-token &&\
	go get &&\
	go build -o /nexus-auth-token &&\
	apk del go git mercurial &&\
	rm -fr /go

ENTRYPOINT ["/nexus-auth-token"]
