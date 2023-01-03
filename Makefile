# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -x -ldflags="-extldflags=-static" -o ./bin/ ./cmd/nfsexportplugin

cleanup:
	kubectl -n csi-nfs-export delete pvc,pod,svc --all --wait=false

clean:
	rm -vfr ./bin/nfsexportplugin

run-local-provisioner: 
	./bin/csi-provisioner --kubeconfig ~/.kube/config -v 5 --csi-address /usr/local/var/run/csi/socket

run-local-plugin:
	go run ./cmd/nfsexportplugin/ -v 5 --endpoint unix:///usr/local/var/run/csi/socket

run-local-nfs-server:
	docker rm -f nfs-ganesha
	docker run --name nfs-ganesha \
		--privileged \
		-d --restart=unless-stopped \
		-v /Users/alexz/nfs:/export \
		daocloud.io/piraeus/volume-nfs-exporter:ganesha
	docker ps | grep nfs-ganesha



re: cleanup clean build
	kubectl delete -f run/controller.yaml || true
	kubectl -n csi-nfs-export wait deployment csi-nfs-export-controller --for=delete --timeout=90s
	kubectl apply -f run/controller.yaml
	kubectl -n csi-nfs-export rollout status deploy csi-nfs-export-controller --timeout=90s
	kubectl -n csi-nfs-export get pod

	kubectl delete -f run/node.yaml || true
	kubectl -n csi-nfs-export wait ds csi-nfs-export-node --for=delete --timeout=90s
	kubectl apply -f run/node.yaml
	kubectl -n csi-nfs-export rollout status ds csi-nfs-export-node --timeout=90s
	kubectl -n csi-nfs-export get pod

test:
	kubectl apply -f example/pvc-dynamic.yaml
	kubectl apply -f example/deployment-dynamic.yaml
	watch kubectl get pod -o wide

untest:
	kubectl delete -f example/deployment-dynamic.yaml
