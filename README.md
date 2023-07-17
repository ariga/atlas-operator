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

### Installation

The Atlas Kubernetes Operator is available as a Helm chart. To install the chart with the release name `atlas-operator`:

```bash
helm install atlas-operator oci://ghcr.io/ariga/charts/atlas-operator
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
  secret/postgres-credentials configured
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
        name: postgresql-credentials
    dir:
      configMapRef:
        name: "migrationdir"
  ```

  We can define a migration directory inlined in the migration resource instead of using a ConfigMap:
  
  ```yaml
  apiVersion: db.atlasgo.io/v1alpha1
  kind: AtlasMigration
  metadata:
    name: atlasmigration-sample
  spec:
    urlFrom:
      secretKeyRef:
        key: url
        name: postgresql-credentials
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

### Support

Need help? File issues on the [Atlas Issue Tracker](https://githb.com/ariga/atlas/issues) or join
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
