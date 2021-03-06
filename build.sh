#!/usr/bin/env bash
set -e
GOGOROOT="${GOPATH}/src/github.com/gogo/protobuf"
GOGOPATH="${GOGOROOT}:${GOGOROOT}/protobuf"
DIRS="./node"
for dir in ${DIRS}; do
    pushd ${dir}
        protoc --proto_path=$GOPATH:$GOGOPATH:./ --gogo_out=. *.proto
    popd
done
