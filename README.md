# The Atlas Kubernetes Operator

Manage your database with Kubernetes using [Atlas](https://atlasgo.io).

### What is Atlas? 

[Atlas](https://atlasgo.io) is a popular open-source schema management tool.
It is designed to help software engineers, DBAs and DevOps practitioners manage their database schemas. 
Users can use the [Atlas DDL](https://atlasgo.io/atlas-schema/sql-resources) (data-definition language)
or [plain SQL](https://atlasgo.io/declarative/apply#sql-schema) to describe the desired database 
schema and use the command-line tool to plan and apply the migrations to their systems.

### What is the Atlas Kubernetes Operator?

Like many other stateful resources, reconciling the desired state of a database with its actual state
can be a complex task that requires a lot of domain knowledge. [Kubernetes Operators](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
were introduced to the Kubernetes ecosystem to help users manage complex stateful resources by codifying 
this domain knowledge into a Kubernetes controller.

The Atlas Kubernetes Operator is a Kubernetes controller that uses [Atlas](https://atlasgo.io) to manage
the schema of your database. The Atlas Kubernetes Operator allows you to define the desired schema of your
and apply it to your database using the Kubernetes API.

### Features

- [x] Support for [declarative migrations](https://atlasgo.io/concepts/declarative-vs-versioned#declarative-migrations)
  for schemas defined in [Plain SQL](https://atlasgo.io/declarative/apply#sql-schema) or 
  [Atlas HCL](https://atlasgo.io/concepts/declarative-vs-versioned#declarative-migrations).
- [X] Detect risky changes such as accidentally dropping columns or tables and define a policy to handle them.
- [X] Support for [versioned migrations](https://atlasgo.io/concepts/declarative-vs-versioned#versioned-migrations).
- [X] Supported databases: MySQL, MariaDB, PostgresSQL, SQLite, TiDB, CockroachDB

### Declarative schema migrations

![](https://atlasgo.io/uploads/images/operator-declarative.png)

The Atlas Kubernetes Operator supports [declarative migrations](https://atlasgo.io/concepts/declarative-vs-versioned#declarative-migrations).
In declarative migrations, the desired state of the database is defined by the user and the operator is responsible
for reconciling the desired state with the actual state of the database (planning and executing `CREATE`, `ALTER`
and `DROP` statements).

### Versioned schema migrations

![](https://atlasgo.io/uploads/k8s/operator/versioned-flow.png)

The Atlas Kubernetes Operator also supports [versioned migrations](https://atlasgo.io/concepts/declarative-vs-versioned#versioned-migrations).
In versioned migrations, the database schema is defined by a series of SQL scripts ("migrations") that are applied
in lexicographical order. The user can specify the version and migration directory to run, which can be located
on the [Atlas Cloud](https://atlasgo.io/cloud/getting-started) or stored as a `ConfigMap` in your Kubernetes
cluster.

### Installation

The Atlas Kubernetes Operator is available as a Helm chart. To install the chart with the release name `atlas-operator`:

```bash
helm install atlas-operator oci://ghcr.io/ariga/charts/atlas-operator --create-namespace --namespace atlas-operator
```

### Configuration

To configure the operator, you can set the following values in the `values.yaml` file:

- `prewarmDevDB`: The Operator always keeps devdb resources around to speed up the migration process. Set this to `false` to disable this feature.

- `extraEnvs`: Used to set environment variables for the operator

```yaml
  extraEnvs: []
  # extraEnvs:
  #   - name: FOO
  #     value: "foo"
  #   - name: ATLAS_TOKEN
  #     valueFrom:
  #       secretKeyRef:
  #         key: ATLAS_TOKEN
  #         name: atlas-token-secret
  #   - name: BAZ
  #     valueFrom:
  #       configMapKeyRef:
  #         key: BAZ
  #         name: configmap-resource
```

- `extraVolumes`: Used to mount additional volumes to the operator

```yaml
  extraVolumes: []
  # extraVolumes:
  #   - name: my-volume
  #     secret:
  #       secretName: my-secret
  #   - name: my-volume
  #     configMap:
  #       name: my-configmap
```

- `extraVolumeMounts`: Used to mount additional volumes to the operator

```yaml
  extraVolumeMounts: []
  # extraVolumeMounts:
  #   - name: my-volume
  #     mountPath: /path/to/mount
  #   - name: my-volume
  #     mountPath: /path/to/mount
```

### Authentication

If you want use use any feature that requires logging in (triggers, functions, procedures, sequence support or SQL Server, ClickHouse, and Redshift drivers), you need to provide the operator with an  Atlas token. You can do this by creating a secret with the token:

```shell
kubectl create secret generic atlas-token-secret \
  --from-literal=ATLAS_TOKEN='aci_xxxxxxx'
```

Then set the `ATLAS_TOKEN` environment variable in the operator's deployment manifest:

```yaml
values:
  extraEnvs:
    - name: ATLAS_TOKEN
      valueFrom:
        secretKeyRef:
          key: ATLAS_TOKEN
          name: atlas-token-secret
```

### Getting started

In this example, we will create a MySQL database and apply a schema to it. After installing the
operator, follow these steps to get started:

1. Create a MySQL database and a secret with an [Atlas URL](https://atlasgo.io/concepts/url)
  to the database:

  ```bash
  kubectl apply -f https://raw.githubusercontent.com/ariga/atlas-operator/master/config/integration/databases/mysql.yaml
  ```
  
  Result:
  
  ```bash
  deployment.apps/mysql created
  service/mysql created
  secret/mysql-credentials created
  ```

2. Create a file named `schema.yaml` containing an `AtlasSchema` resource to define the desired schema:

  ```yaml
  apiVersion: db.atlasgo.io/v1alpha1
  kind: AtlasSchema
  metadata:
    name: atlasschema-mysql
  spec:
    urlFrom:
      secretKeyRef:
        key: url
        name: mysql-credentials
    schema:
      sql: |
        create table users (
          id int not null auto_increment,
          name varchar(255) not null,
          email varchar(255) unique not null,
          short_bio varchar(255) not null,
          primary key (id)
        );
  ```

3. Apply the schema:

  ```bash
  kubectl apply -f schema.yaml
  ```
  
  Result:
  ```bash
  atlasschema.db.atlasgo.io/atlasschema-mysql created
  ```

4. Check that our table was created:

  ```bash
  kubectl exec -it $(kubectl get pods -l app=mysql -o jsonpath='{.items[0].metadata.name}') -- mysql -uroot -ppass -e "describe myapp.users"
  ```
  
  Result:
  
  ```bash
  +-----------+--------------+------+-----+---------+----------------+
  | Field     | Type         | Null | Key | Default | Extra          |
  +-----------+--------------+------+-----+---------+----------------+
  | id        | int          | NO   | PRI | NULL    | auto_increment |
  | name      | varchar(255) | NO   |     | NULL    |                |
  | email     | varchar(255) | NO   | UNI | NULL    |                |
  | short_bio | varchar(255) | NO   |     | NULL    |                |
  +-----------+--------------+------+-----+---------+----------------+
  ```
  
Hooray! We applied our desired schema to our target database.

Now, let's try versioned migrations with a PostgreSQL database.

1. Create a PostgresQL database and a secret with an [Atlas URL](https://atlasgo.io/concepts/url)
  to the database:

  ```bash
  kubectl apply -f https://raw.githubusercontent.com/ariga/atlas-operator/master/config/integration/databases/postgres.yaml
  ```
  
  Result:

  ```bash
  deployment.apps/postgres created
  service/postgres unchanged
  ```

2. Create a file named `migrationdir.yaml` to define your migration directory

  ```yaml
  apiVersion: v1
  kind: ConfigMap
  metadata:
    name: migrationdir
  data:
    20230316085611.sql: |
      create sequence users_seq;
      create table users (
        id int not null default nextval ('users_seq'),
        name varchar(255) not null,
        email varchar(255) unique not null,
        short_bio varchar(255) not null,
        primary key (id)
      );
    atlas.sum: |
      h1:FwM0ApKo8xhcZFrSlpa6dYjvi0fnDPo/aZSzajtbHLc=
      20230316085611.sql h1:ldFr73m6ZQzNi8q9dVJsOU/ZHmkBo4Sax03AaL0VUUs=
  ``` 

3. Create a file named `atlasmigration.yaml` to define your migration resource that links to the migration directory.

  ```yaml
  apiVersion: db.atlasgo.io/v1alpha1
  kind: AtlasMigration
  metadata:
    name: atlasmigration-sample
  spec:
    urlFrom:
      secretKeyRef:
        key: url
        name: postgres-credentials
    dir:
      configMapRef:
        name: "migrationdir"
  ```

  Alternatively, we can define a migration directory inlined in the migration resource instead of using a ConfigMap:
  
  ```yaml
  apiVersion: db.atlasgo.io/v1alpha1
  kind: AtlasMigration
  metadata:
    name: atlasmigration-sample
  spec:
    urlFrom:
      secretKeyRef:
        key: url
        name: postgres-credentials
    dir:
      local:
        20230316085611.sql: |
          create sequence users_seq;
          create table users (
            id int not null default nextval ('users_seq'),
            name varchar(255) not null,
            email varchar(255) unique not null,
            short_bio varchar(255) not null,
            primary key (id)
          );
        atlas.sum: |
          h1:FwM0ApKo8xhcZFrSlpa6dYjvi0fnDPo/aZSzajtbHLc=
          20230316085611.sql h1:ldFr73m6ZQzNi8q9dVJsOU/ZHmkBo4Sax03AaL0VUUs=
  ```
4. Apply migration resources:

  ```bash
  kubectl apply -f migrationdir.yaml
  kubectl apply -f atlasmigration.yaml
  ```
  
  Result:
  ```bash
  atlasmigration.db.atlasgo.io/atlasmigration-sample created
  ```

5. Check that our table was created:

  ```
  kubectl exec -it $(kubectl get pods -l app=postgres -o jsonpath='{.items[0].metadata.name}') -- psql -U root -d postgres -c "\d+ users"
  ```

  Result:

  ```bash
    Column   |          Type          | Collation | Nullable |            Default             | Storage  | Compression | Stats target | Description
  -----------+------------------------+-----------+----------+--------------------------------+----------+-------------+--------------+-------------
   id        | integer                |           | not null | nextval('users_seq'::regclass) | plain    |             |              |
   name      | character varying(255) |           | not null |                                | extended |             |              |
   email     | character varying(255) |           | not null |                                | extended |             |              |
   short_bio | character varying(255) |           | not null |                                | extended |             |              |
  ```
  
  Please refer to [this link](https://atlasgo.io/integrations/kubernetes/operators/versioned) to explore the supported API for versioned migrations.

### API Reference

Example resource: 

```yaml
apiVersion: db.atlasgo.io/v1alpha1
kind: AtlasSchema
metadata:
  name: atlasschema-mysql
spec:
  urlFrom:
    secretKeyRef:
      key: url
      name: mysql-credentials
  policy:
    # Fail if the diff planned by Atlas contains destructive changes.
    lint:
      destructive:
        error: true
    diff:
      # Omit any DROP INDEX statements from the diff planned by Atlas.
      skip:
        drop_index: true
  schema:
    sql: |
      create table users (
        id int not null auto_increment,
        name varchar(255) not null,
        primary key (id)
      );
  exclude:
    - ignore_me
```

This resource describes the desired schema of a MySQL database. 
* The `urlFrom` field is a reference to a secret containing an [Atlas URL](https://atlasgo.io/concepts/url) 
 to the target database. 
* The `schema` field contains the desired schema in SQL. To define the schema in HCL instead of SQL, use the `hcl` field:
  ```yaml
  spec:
    schema:
      hcl: |
        table "users" {
          // ...
        }
  ```
  To learn more about defining SQL resources in HCL see [this guide](https://atlasgo.io/atlas-schema/sql-resources).
* The `policy` field defines different policies that direct the way Atlas will plan and execute schema changes.
  * The `lint` policy defines a policy for linting the schema. In this example, we define a policy that will fail
    if the diff planned by Atlas contains destructive changes.
  * The `diff` policy defines a policy for planning the schema diff. In this example, we define a policy that will
    omit any `DROP INDEX` statements from the diff planned by Atlas.

### Version checks

The operator will periodically check for new versions and security advisories related to the operator.
To disable version checks, set the `SKIP_VERCHECK` environment variable to `true` in the operator's
deployment manifest.

### Troubleshooting

In successful reconciliation, the conditon status will look like this:

```yaml
Status:
  Conditions:
    Last Transition Time: 2024-03-20T09:59:56Z
    Message: ""
    Reason: Applied
    Status: True
    Type: Ready
  Last Applied: 1710343398
  Last Applied Version: 20240313121148
  observed_hash: d5a1c1c08de2530d9397d4
```

In case of an error, the condition `status` will be set to false and `reason` field will contain the type of error that occurred (e.g. `Reconciling`, `ReadingMigrationData`, `Migrating`, etc.). To get more information about the error, you can check the `message` field.

**For AtlasSchema resource:**
| Reason | Description |
| ------ | ----------- |
| Reconciling | The operator is reconciling the desired state with the actual state of the database |
| ReadSchema | There was an error about reading the schema from ConfigMap or database credentials |
| GettingDevDB | failed to get the devdb resource, in case we are using the devdb for nomalization |
| VerifyingFirstRun | occurred when a first run of the operator that contain destructive changes |
| LintPolicyError | occurred when the lint policy is violated |
| ApplyingSchema | failed to apply to database |

**For AtlasMigration resource:** 

| Reason | Description |
| ------ | ----------- |
| Reconciling | The operator is reconciling the desired state with the actual state of the database |
| GettingDevDB | failed to get the devdb resource, in case we are using the devdb for compute plan for migration |
| ReadingMigrationData | failed to read the migration directory from `ConfigMap`, Atlas Cloud or invalid database credentials |
| ProtectedFlowError | occurred when the migration is protected and the operator is not able to apply it |
| ApprovalPending | The migration is protected and waiting for approval on Atlas Cloud |
| Migrating | failed to migrate to database |

### Support

Need help? File issues on the [Atlas Issue Tracker](https://github.com/ariga/atlas/issues) or join
our [Discord](https://discord.gg/zZ6sWVg6NT) server.

### Development

Start [MiniKube](https://minikube.sigs.k8s.io/docs/start/)

```bash
minikube start
```

Install CRDs

```bash
make install
```

Start [Skaffold](https://skaffold.dev/)
```
skaffold dev
```

### License

The Atlas Kubernetes Operator is licensed under the [Apache License 2.0](LICENSE).
