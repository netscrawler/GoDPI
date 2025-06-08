mkdir target

docker build -t builder .

docker run --rm -v $(pwd)/target:/host-target builder sh -c "cp -r /target/* /host-target/"


