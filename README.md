# The Atlas Kubernetes Operator

Let your Kubernetes cluster manage your database using [Atlas](https://atlasgo.io).

### What is Atlas? 

[Atlas](https://github.com/ariga/atlas) is a popular open-source schema management tool.
Atlas is designed to help software engineers, DBAs and DevOps practitioners manage their database schemas. 
Atlas users can use the [Atlas DDL](https://atlasgo.io/atlas-schema/sql-resources) (data-definition language)
or [plain SQL](https://atlasgo.io/declarative/apply#sql-schema) to describe the desired database 
schema and use the command-line tool to plan and apply the migrations to their systems.

### What is the Atlas Kubernetes Operator?

Like many other stateful resources, reconciling the desired state of a database with its actual state
can be a complex task that requires a lot of domain knowledge.  [Kubernetes Operators](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
were introduced to the Kubernetes ecosystem to help users manage complex stateful resources by codifying 
this domain knowledge into a Kubernetes controller.

The Atlas Kubernetes Operator is a Kubernetes controller that uses [Atlas](https://atlasgo.io) to manage
the schema of your database. The Atlas Kubernetes Operator allows you to define the desired schema of your
database in plain SQL or in [Atlas HCL](https://atlasgo.io/atlas-schema/sql-resources) and apply it to your
database using the Kubernetes API.

### Features

- [x] Support for [declarative migrations](https://atlasgo.io/concepts/declarative-vs-versioned#declarative-migrations)
  for schemas defined in [Plain SQL](https://atlasgo.io/declarative/apply#sql-schema) or 
  [Atlas HCL](https://atlasgo.io/concepts/declarative-vs-versioned#declarative-migrations).
- [ ] Detect risky changes such as accidentally dropping columns or tables and define a policy to handle them. (Coming Soon)   
- [ ] Support for [versioned migrations](https://atlasgo.io/concepts/declarative-vs-versioned#versioned-migrations). (Coming Soon)
- [X] Supported databases: MySQL, MariaDB, PostgresSQL, SQLite, TiDB, CockroachDB

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
  kubectl apply -f https://raw.githubusercontent.com/ariga/atlas-operator/master/config/integration/mysql.yaml
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

### Support

Need help? File issues on the [Atlas Issue Tracker](https://githb.com/ariga/atlas/issues) or join
our [Discord](https://discord.gg/zZ6sWVg6NT) server.

### Development

Start [MiniKube](https://minikube.sigs.k8s.io/docs/start/)

```bash
minikube start
```

Start [Skaffold](https://skaffold.dev/)
```
skaffold dev
```

### License

The Atlas Kubernetes Operator is licensed under the [Apache License 2.0](LICENSE).