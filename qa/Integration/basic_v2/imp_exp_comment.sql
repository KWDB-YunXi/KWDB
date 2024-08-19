--test_case001 basic export and import database with comment;
--testmode 1n 5c
create ts database test;
use test;
create table test.tb1(k_timestamp timestamptz not null, e1 int2, e2 int4, e3 int8, e4 float4, e5 float8, e6 bool, e7 char(20), e8 nchar(20), e9 varbytes(20), e10 double) tags (tag1 int2 not null, tag2 int4 not null, tag3 int8 not null, tag4 float4, tag5 float8, tag6 bool, tag7 char(20), tag8 nchar(20), tag9 varbytes(20), tag10 double) primary tags(tag1, tag2, tag3);
insert into test.tb1 values('2024-01-01 00:00:01+00:00',663,620,901,7463.861328125,-1551.4947464030101,true,'x','o','\x30',225.31828421061618,820,139,851,3052.771728515625,-3061.167301514549,true,'w','Z','\x38',1632.308420147181);
insert into test.tb1 values('2024-01-01 00:00:01+00:00',663,620,901,7463.861328125,-1551.4947464030101,true,'x','o','\\test',225.31828421061618,495,736,420,3052.771728515625,-3061.167301514549,true,'w','Z','\x38',1632.308420147181);
insert into test.tb1 values('2024-01-01 00:00:02+00:00',500,324,821,-3514.2734375,2907.959323289191,false,'g','R','\x39',1942.0105699072847,865,577,987,-6812.10791015625,5215.895202662417,true,'U','i','\x45',-6363.044280492493);
insert into test.tb1 values('2024-01-01 00:00:02+00:00',500,324,821,-3514.2734375,2907.959323289191,false,'g','R','测试',1942.0105699072847,820,139,851,-6812.10791015625,5215.895202662417,true,'U','i','\x45',-6363.044280492493);
insert into test.tb1 values('2024-01-01 00:00:04+00:00',666,119,807,9944.78125,-7359.134805999276,true,'A','H','\x42',-238.10581074656693,495,736,420,659.4307861328125,-349.5548293794309,false,'m','o','\x36',3778.0368072157435);
insert into test.tb1 values('2024-01-01 00:00:04+00:00',666,119,807,9944.78125,-7359.134805999276,true,'A','H','byte',-238.10581074656693,865,577,987,659.4307861328125,-349.5548293794309,false,'m','o','\x36',3778.0368072157435);
insert into test.tb1 values('2024-01-01 00:00:05+00:00',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,865,577,987,NULL,NULL,NULL,NULL,NULL,NULL,NULL);
create table test.tb2(k_timestamp timestamptz not null, e1 int2, e2 int4, e3 int8, e4 float4, e5 float8, e6 bool, e7 char(20), e8 nchar(20), e9 varbytes(20), e10 double) tags (tag1 int2 not null, tag2 int4 not null, tag3 int8 not null, tag4 float4, tag5 float8, tag6 bool, tag7 char(20), tag8 nchar(20), tag9 varbytes(20), tag10 double) primary tags(tag1, tag2, tag3);
insert into test.tb2 values('2024-01-01 00:00:00+00:00',853,102,126,-269.9822082519531,-6310.8133409285365,false,'z','N','\x33',3262.9201366604448,431,625,332,2097.56640625,-8080.921804629673,true,'D','a','\x46',-9985.633213887826);
insert into test.tb2 values('2024-01-01 00:00:03+00:00',926,422,192,168.82395935058594,-6538.5148542374245,false,'K','V','\x37',7827.629938345075,502,737,13,9694.1708984375,-4820.426745015345,true,'Y','Q','\x36',-6651.346270440837);
comment on database test is 'database for comment';
export into csv "nodelocal://1/dbcomment/db1" from database test with comment;
use defaultdb;
drop database test cascade;
show databases with comment;
import database csv data ("nodelocal://1/dbcomment/db1") with comment;
use test;
show databases with comment;

--test_case002 database table column with comment;
--testmode 1n 5c
comment on table test.tb1 is 'table for comment';
comment on column test.tb1.e1 is 'e1 column for comment';
export into csv "nodelocal://1/dbcomment/db2" from database test with comment;
use defaultdb;
drop database test cascade;
import database csv data ("nodelocal://1/dbcomment/db2") with comment;
use test;
show databases with comment;
show tables with comment;
show columns from test.tb1 with comment;

--test_case003  datable table column with comment=false;
--testmode 1n 5c
export into csv "nodelocal://1/dbcomment/db3" from database test;
use defaultdb;
drop database test cascade;
import database csv data ("nodelocal://1/dbcomment/db3");
use test;
show databases with comment;
show tables with comment;
show columns from test.tb1 with comment;
use defaultdb;
drop database test cascade;

--test_case004 column with comment;
--testmode 1n 5c
create ts database def;
use def;
create table deftb(k_timestamp timestamptz not null, e1 int2, e2 int4, e3 int8, e4 float4, e5 float8, e6 bool, e7 char(20), e8 nchar(20), e9 varbytes(20), e10 double) tags (tag1 int2 not null, tag2 int4 not null, tag3 int8 not null, tag4 float4, tag5 float8, tag6 bool, tag7 char(20), tag8 nchar(20), tag9 varbytes(20), tag10 double) primary tags(tag1, tag2, tag3);
insert into deftb values('2024-01-01 00:00:01+00:00',663,620,901,7463.861328125,-1551.4947464030101,true,'x','o','\x30',225.31828421061618,820,139,851,3052.771728515625,-3061.167301514549,true,'w','Z','\x38',1632.308420147181);
insert into deftb values('2024-01-01 00:00:01+00:00',663,620,901,7463.861328125,-1551.4947464030101,true,'x','o','\\test',225.31828421061618,495,736,420,3052.771728515625,-3061.167301514549,true,'w','Z','\x38',1632.308420147181);
insert into deftb values('2024-01-01 00:00:02+00:00',500,324,821,-3514.2734375,2907.959323289191,false,'g','R','\x39',1942.0105699072847,865,577,987,-6812.10791015625,5215.895202662417,true,'U','i','\x45',-6363.044280492493);
insert into deftb values('2024-01-01 00:00:02+00:00',500,324,821,-3514.2734375,2907.959323289191,false,'g','R','测试',1942.0105699072847,820,139,851,-6812.10791015625,5215.895202662417,true,'U','i','\x45',-6363.044280492493);
insert into deftb values('2024-01-01 00:00:04+00:00',666,119,807,9944.78125,-7359.134805999276,true,'A','H','\x42',-238.10581074656693,495,736,420,659.4307861328125,-349.5548293794309,false,'m','o','\x36',3778.0368072157435);
insert into deftb values('2024-01-01 00:00:04+00:00',666,119,807,9944.78125,-7359.134805999276,true,'A','H','byte',-238.10581074656693,865,577,987,659.4307861328125,-349.5548293794309,false,'m','o','\x36',3778.0368072157435);
insert into deftb values('2024-01-01 00:00:05+00:00',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,865,577,987,NULL,NULL,NULL,NULL,NULL,NULL,NULL);
comment on column deftb.e1 is 'e1 column for comment';
export into csv "nodelocal://1/dbcomment/db4" from table def.deftb with comment;
drop table deftb;
import table create using 'nodelocal://1/dbcomment/db4/meta.sql' csv data ('nodelocal://1/dbcomment/db4') with comment;
show tables with comment;
show columns from deftb with comment;

--test_case005 table column with comment;
--testmode 1n 5c
comment on table deftb is 'table for comment';
export into csv "nodelocal://1/dbcomment/db5" from table def.deftb with comment;
drop table deftb;
import table create using 'nodelocal://1/dbcomment/db5/meta.sql' csv data ('nodelocal://1/dbcomment/db5') with comment;
show databases with comment;
show tables with comment;
show columns from deftb with comment;

--test_case006 table column with comment = false;
--testmode 1n 5c
comment on table deftb is 'table for comment';
export into csv "nodelocal://1/dbcomment/db6" from table def.deftb;
drop table deftb;
import table create using 'nodelocal://1/dbcomment/db6/meta.sql' csv data ('nodelocal://1/dbcomment/db6');
show databases with comment;
show tables with comment;
show columns from deftb with comment;
use defaultdb;
drop database def cascade;