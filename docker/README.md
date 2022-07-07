# cPacket Docker packaging

Adds a hastily assembled Dockerfile to package the output of the go build of
telegraf. To use:

1. From the root directory, make the amd64 debian package: 
   `make package include_packages="amd64.deb"`
2. Build & push the docker image
```
  docker build -t docker.artifactory.int.cpacket.com/cpacket/telegraf:bfa2e66e -f ./docker/Dockerfile .
  docker push docker.artifactory.int.cpacket.com/cpacket/telegraf:bfa2e66e
```

