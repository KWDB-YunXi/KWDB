drop database tsdb;
create ts database tsdb;
use tsdb;
create table t1(ts timestamp not null,a int, b int) tags(tag1 int not null, tag2 int) primary tags(tag1);
insert into t1 values(1705028908000,11,22,33,44);
insert into t1 values(1705028909000,22,33,33,44);

select * from tsdb.t1;
select t1.* from tsdb.t1;
select tt.* from tsdb.t1 tt;
select a from tsdb.t1;
select ts from tsdb.t1;
select ts, tag1 from tsdb.t1;
select a, tag2 from tsdb.t1;
select * from tsdb.t1 where tag1<1012;
select * from tsdb.t1 where tag1=33;
select * from tsdb.t1 where a<1012;

select a+tag2 from tsdb.t1;
select a from tsdb.t1 where tag1 > 10;

select a from tsdb.t1 where tag1<1012;
select a from tsdb.t1 where tag1=33;
select a from tsdb.t1 where a<1012;

select tag1 from tsdb.t1 where tag1<1012;
select tag1 from tsdb.t1 where tag1=33;
select tag1 from tsdb.t1 where a<1012;

select a+tag1 from tsdb.t1 where tag1<1012;
select a+tag1 from tsdb.t1 where tag1=33;
select a+tag1 from tsdb.t1 where a<1012;

SELECT variance(LE)//10 FROM (SELECT max(a) LE FROM tsdb.t1 GROUP BY a);

explain select avg(a) from tsdb.t1;
explain select avg(a) from tsdb.t1 where tag1 = 33;
explain select avg(a) from tsdb.t1 group by tag1;

select avg(a) from tsdb.t1;
select avg(a) from tsdb.t1 where tag1 = 33;
select avg(a) from tsdb.t1 group by tag1;

select count(distinct a) from tsdb.t1;
select count(distinct a), sum(b) from tsdb.t1;

drop database tsdb cascade;

use defaultdb;drop database if exists test_select_first cascade;
create ts database test_select_first;
create table test_select_first.tb(k_timestamp timestamptz not null,e1 timestamptz,e2 int2,e3 int,e4 int8,e5 float4,e6 float8,e7 bool,e8 char,e9 char(100),e10 nchar,e11 nchar(255),e12 varchar,e13 varchar(254),e14 varchar(4096),e15 nvarchar,e16 nvarchar(255),e17 nvarchar(4096),e18 varbytes,e19 varbytes(100),e20 varbytes,e21 varbytes(254),e22 varbytes(4096)) tags (t1 int2 not null,t2 int,t3 int8,t4 bool not null,t5 float4,t6 float8,t7 char,t8 char(100) not null,t9 nchar,t10 nchar(254),t11 varchar,t12 varchar(128) not null,t13 varbytes,t14 varbytes(100),t15 varbytes,t16 varbytes(255)) primary tags(t1,t4,t8,t12);
create table test_select_first.tb2(k_timestamp timestamptz not null,e1 timestamptz,e2 int2,e3 int,e4 int8,e5 float4,e6 float8,e7 bool,e8 char,e9 char(100),e10 nchar,e11 nchar(255),e12 varchar,e13 varchar(254),e14 varchar(4096),e15 nvarchar,e16 nvarchar(255),e17 nvarchar(4096),e18 varbytes,e19 varbytes(100),e20 varbytes,e21 varbytes(254),e22 varbytes(4096)) tags (t1 int2,t2 int not null,t3 int8,t4 bool,t5 float4,t6 float8,t7 char not null,t8 char(100),t9 nchar not null,t10 nchar(254) not null,t11 varchar,t12 varchar(128),t13 varbytes,t14 varbytes(100),t15 varbytes,t16 varbytes(255)) primary tags(t2,t7,t9,t10);
create table test_select_first.tb3(k_timestamp timestamptz not null,e1 timestamptz,e2 int2,e3 int,e4 int8,e5 float4,e6 float8,e7 bool,e8 char,e9 char(100),e10 nchar,e11 nchar(255),e12 varchar,e13 varchar(254),e14 varchar(4096),e15 nvarchar,e16 nvarchar(255),e17 nvarchar(4096),e18 varbytes,e19 varbytes(100),e20 varbytes,e21 varbytes(254),e22 varbytes(4096)) tags (t1 int2,t2 int not null,t3 int8 not null,t4 bool,t5 float4,t6 float8,t7 char,t8 char(100),t9 nchar,t10 nchar(254),t11 varchar not null,t12 varchar(128),t13 varbytes not null,t14 varbytes(100) not null,t15 varbytes,t16 varbytes(255)) primary tags(t3,t11,t2);
insert into test_select_first.tb values ('2020-11-06 17:10:23','1970-01-01 08:00:00',700,7000,70000,700000.707,7000000.1010101,true,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,1,null,7000,false,70.7077,700.5675675,'a','test测试！！！@TEST1','e','\a',null,'vvvaa64_1','b','test测试1023_1','vwwws测试_1','aaabbb'),('2020-11-06 17:10:55.123','2019-12-06 18:10:23',100,3000,40000,600000.60612,4000000.4040404,false,' ',' ',' ',' ',' ',' ',' ',null,'','','','','','',null,-32768,-2147483648,-9223372036854775808,false,-922.123,100.111111,'b','test测试！！！@TEST1 ','','test测试！TEST1xaa','\0test查询  @TEST1\0','e','y','test@@测试！1023_1','vwwws测试_1','cccddde'),('2022-05-01 12:10:25','2020-05-01 20:30:00',500,5000,50000,-500000.505,-5000000.505055,true,'h', 'ar2  ', 'c', 'r255测试2()*&^%{}','\\\\', 'v255测试1cdf~#   ', 'lengthis4096  测试%&!','ar-1', 'ar255()*&^%{}  ','6_1测试1&^%{} ','y', 's1023_1','ytes_2', null,b'\xcc\xcc\xdd\xdd',4,400,4000,false,50.555,500.578578,'d','\\test测试！！！@TEST1','e','test测试！T  EST1xaa','查询查询 ',e'\\\\','e','es1023_2','s_ 4','ww4096_2'),('2022-05-01 12:10:23.456','2020-05-01 17:00:00',500,5000,60000,500000.505555,5000000.505055,false,'c','test测试！！！@TEST1 ','n','类型测试1()*  ','testTest  ','e','40964096 ','@TEST1  ','abc255测试1()*&^%{}','deg4096测试1(','b','查询1023_2','tes_测试1',b'\xaa\xbb\xcc',b'\xaa\xaa\xbb\xbb',32767,2147483647,9223372036854775807,true,922.123,500.578578,'','     ',' ','abc','\0test查询！！！@TEST1\0','64_3','t','es1023_2','f','tes4096_2'),('2022-05-01 12:10:31.22','2020-05-01 22:30:11',500,5000,50000,-500000.505,-5000000.505055,true,'h', 'ar2  ', 'c', 'r255测试2()*&^%{}','\\', 'v2551cdf~#   ', '  测试%&!','ar-1', 'ar255()*&^%{}  ','6_1测试1&^%{} ','y', 's1023_1','ytes_2', null,b'\xcc\xcc\xdd\xdd',4,300,300,false,60.666,600.678,'','\\test测试！！！@TEST1',' ','test测试！T  EST1xaa','查询查询 ',e'\\\\','','    ','','  '),('2023-05-10 09:08:19.22','2021-05-10 09:04:18.223',600,6000,60000,600000.666,666660.101011,true,'r', 'a r3', 'a', 'r255测试1(){}','varchar  中文1', null, 'hof4096查询test%%&!   ',null, 'ar255{}', 'ar4096测试1%{}','e','es1023_0', null, b'\xbb\xee\xff', null,5,null,6000,true,60.6066,600.123455,'a','test测试！！！@TEST1','e','a',null,'测试测试 ','b','test测试10_1','vwwws中文_1',null),('2023-05-10 09:15:15.783','2021-06-10 06:04:15.183',500,5000,60000,500000.505555,5000000.505055,false,'c','test测试！！！@TEST1 ','n','类型测试1()*  ',null,null,'测试测试 ','@TEST1  ','abc255测试1()*&^%{}','deg4096测试1(','b','查询1023_2','tes_测试1',b'\xaa\xaa\xaa',b'\xbb\xcc\xbb\xbb',7,200,2000,true,-10.123,500.578578,'c','test测试！！！@TEST1  ','g','abc','\0test查询！！！@TEST1\0','64_3','t','es1023_2','f','tes4096_2'),('2023-05-10 09:08:18.223','2021-06-01 10:00:00',100,3000,40000,600000.60612,4000000.4040404,false,'r', '\a r3', 'a', 'r255测试1{}','varchar  中文1', null, 'hof4096查询test%&!   ',null, 'ar255{}', 'ar96测试1%{}','e','es1023_0', null, b'\xcc\xee\xdd', null,6,100,1000,false,-10.123,100.111111,'b','\\TEST1 ','f','测试！TEST1xaa','5555 5','  bdbd','y','test@测试！10_1','vwwws_1','cddde');
insert into test_select_first.tb2 values ('2023-05-10 09:08:18.223','2021-05-10 09:04:18.223',600,6000,60000,600000.666,666660.101011,true,'r', 'a r3', 'a', 'r255测试1(){}','中文  中文', null, 'hof4096查询test%%&!   ',null, 'ar255{}', 'ar4096测试1%{}','e','es1023_0', null, b'\xbb\xee\xff', null,5,600,6000,false,60.6066,600.123455,'a','test测试！！！@TEST1','e','\a',null,'chch4_1','b','test测试10_1','vwwws中文_1',null),('2023-05-10 09:08:19.22','2021-06-01 10:00:00',100,3000,40000,600000.60612,4000000.4040404,false,'r', '\a r3', 'a', 'r255测试1{}','varchar  中文1', null, 'hof4096查询test%&!   ',null, 'ar255{}', 'ar96测试1%{}','e','es1023_0', null, b'\xcc\xee\xdd', null,6,100,1000,true,-10.123,100.111111,'b','\\TEST1 ','f','测试！TEST1xaa','test查询  @TEST1\0','bd64_1','y','test@测试！10_1','vwwws_1','cddde');
insert into test_select_first.tb3 values ('2024-05-10 23:23:23.783','2024-06-10 16:16:15.183',800,8000,80000,800000.808888,8000000.808088,true,'d','test测试！！！@TEST1 ','d','类型测试1()*  ',null,null,'255测试1cdf~# ','@TEST1  ','abc255测试1()*&^%{}','deg4096测试1(','b','查询1023_2','tes_测试1',b'\xaa\xaa\xaa',b'\xbb\xcc\xbb\xbb',7,200,2000,false,-10.123,500.578578,'c','test测试！！！@TEST1  ','g','abc','\0test查询！！！@TEST1\0','64_3','t','es1023_2','f','tes4096_2'),('2023-05-10 09:15:15.783','2021-06-10 06:04:15.183',500,5000,60000,500000.505555,5000000.505055,false,'c','test测试！！！@TEST1 ','n','类型测试1()*  ',null,null,'255测试1cdf~# ','@TEST1  ','abc255测试1()*&^%{}','deg4096测试1(','b','查询1023_2','tes_测试1',b'\xaa\xaa\xaa',b'\xbb\xcc\xbb\xbb',8,800,8000,false,-20.123,800.578578,'d','test测试！！！@TEST1  ','d','ddd','\0test查询！！！@TEST1\0','64_3','t','es1023_2','f','tes4096_2');


select first(t3),first(t11),first(t13),first(t14) from test_select_first.tb3 group by t14 having t14 in('\x6573313032335f320000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000',null);

select first(t6),first(t16),first(t13),first(t14) from test_select_first.tb3 group by t14 having t14 in('\x6573313032335f320000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000',null);

use defaultdb;drop database if exists test_select_first cascade;
