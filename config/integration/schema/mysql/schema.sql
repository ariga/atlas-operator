create table users (
  id int not null auto_increment,
  name varchar(255) not null,
  email varchar(255) unique not null,
  short_bio varchar(255) not null,
  phone varchar(255) not null,
  primary key (id)
);
