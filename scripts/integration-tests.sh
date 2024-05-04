#!/bin/bash -e
# Copyright 2024 The Atlas Operator Authors.
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

mysql_exec() {
  if [ -z "$1" ]; then
    echo "Usage: mysql_exec <query>"
    exit 1
  fi
  _mysql_pod=$(kubectl get pods -l app=mysql -o jsonpath='{.items[0].metadata.name}')
  kubectl exec $_mysql_pod -- \
    mysql -uroot -h 127.0.0.1 -ppass -e "$1"
}

mysql_reset() {
  if [ -z "$1" ]; then
    echo "Usage: mysql_reset <description>"
    exit 1
  fi
  echo ""
  echo "---------------------------------"
  echo "$1"
  echo "---------------------------------"
  echo ""
  # Delete the pods to reset the database.
  kubectl delete pods -l app=mysql 1>/dev/null
  # Wait for the pods to be ready.
  kubectl wait --for condition=ready pods -l app=mysql --timeout=60s 1>/dev/null
}

k8s_dircfg() {
  if [ -z "$1" ] || [ -z "$2" ]; then
    echo "Usage: k8s_dircfg <dir> <cfg>"
    exit 1
  fi
  _ns="${3:-default}"
  kubectl create configmap $2 --from-file=$1 \
    --dry-run=client -o yaml | kubectl apply -n $_ns -f -
}

# Reset the environment to ensure a clean state.
kubectl set env -n atlas-operator-system deployment/atlas-operator-controller-manager \
  PREWARM_DEVDB=true

cd ./config/integration

# Bring up the database resources
kubectl apply -f ./databases

mysql_reset "Test the atlas schema controller"
# Create a table not described in the desired schema but excluded from it.
mysql_exec "create table myapp.ignore_me (c int);"
# Apply the desired schema and wait for it to be ready.
kubectl apply -f ./schema
kubectl wait --for=condition=ready --timeout=120s atlasschemas --all
# Expect the excluded table to be present.
mysql_exec "describe myapp.ignore_me"
# Update sql schema configmap
kubectl create configmap mysql-schema \
  --from-file=./schema/mysql \
  --dry-run=client -o yaml | kubectl apply -f -
sleep 1 # Wait for the migration to be applied.
kubectl wait --for=condition=ready --timeout=120s atlasschemas --all
echo ""
echo "Expect the new column to be present"
mysql_exec "SHOW COLUMNS FROM myapp.users LIKE 'phone';" | grep -q 'phone'
echo ""
echo "Expect the devdb deployment is scaled to 1"
kubectl get deployment atlasschema-mysql-atlas-dev-db \
  -o=jsonpath='{.spec.replicas}' | grep -q '1'

mysql_reset "Test prewarm_devdb flag"
# SET PREWARM_DEVDB to false
kubectl set env -n atlas-operator-system deployment/atlas-operator-controller-manager \
  PREWARM_DEVDB=false
# Apply the desired schema and wait for it to be ready.
kubectl delete -f ./schema
kubectl apply -f ./schema
kubectl wait --for=condition=ready --timeout=120s atlasschemas --all
echo ""
echo "Expect the devdb deployment is scaled to 0"
kubectl get deployment atlasschema-mysql-atlas-dev-db \
  -o=jsonpath='{.spec.replicas}' | grep -q '0'

cd ./migration

mysql_reset "Test the atlas migration controller"
k8s_dircfg ./mysql-migrations migration-dir
kubectl apply -f ./mysql_migration.yaml
kubectl wait --for=condition=ready --timeout=120s atlasmigrations --all
echo ""
echo "Expected the atlas_schema_revisions table to be present"
mysql_exec "describe myapp.atlas_schema_revisions"
echo "Add a new column to the table"
EDITOR="echo 'ALTER TABLE posts ADD COLUMN created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP;' >" \
  atlas migrate new --dir=file://./mysql-migrations --edit
k8s_dircfg ./mysql-migrations migration-dir
kubectl wait --for=condition=ready --timeout=120s atlasmigrations --all
echo ""
echo "Expected the new column to be present"
mysql_exec "SHOW COLUMNS FROM myapp.posts LIKE 'created_at';" | grep -q 'created_at'
echo ""
