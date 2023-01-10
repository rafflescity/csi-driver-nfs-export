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

clean:
	kubectl delete -f run/controller.yaml || true
	kubectl delete -f run/node.yaml || true
	kubectl -n csi-nfs-export wait po -l nfs-export.csi.k8s.io/app --for=delete --timeout=90s
	rm -vfr ./bin/nfsexportplugin

deploy:
	kubectl apply -f run/controller.yaml
	kubectl apply -f run/node.yaml
	kubectl -n csi-nfs-export rollout status deploy,ds --timeout=90s
	kubectl -n csi-nfs-export get pod

re: untest clean build deploy

test:
	kubectl apply -f example/pvc-dynamic.yaml
	kubectl apply -f example/deployment-dynamic.yaml
	watch kubectl get pod -o wide

untest:
	kubectl delete -f example/deployment-dynamic.yaml || true
	kubectl delete -f example/pvc-dynamic.yaml || true
	kubectl delete pod,pvc,svc -l nfs-export.csi.k8s.io/id

logj:
	kubectl -n csi-nfs-export get po -l app=csi-nfs-export-node --field-selector spec.nodeName=jammy -o name \
	| xargs -tI % kubectl -n csi-nfs-export logs -f % nfs-export

logf:
	kubectl -n csi-nfs-export get po -l app=csi-nfs-export-node --field-selector spec.nodeName=focal -o name \
	| xargs -tI % kubectl logs -n csi-nfs-export -f % nfs-export

logc:
	kubectl logs -f deploy/csi-nfs-export-controller nfs-export -n csi-nfs-export


run-local-nfs-server:
	docker rm -f nfs-ganesha
	docker run --name nfs-ganesha \
		--privileged \
		-d --restart=unless-stopped \
		-v /Users/alexz/nfs:/export \
		daocloud.io/piraeus/volume-nfs-exporter:ganesha
	docker ps | grep nfs-ganesha
