-- create
SET CLUSTER SETTING server.advanced_distributed_operations.enabled = true;
SET cluster setting ts.rows_per_block.max_limit=10;
SET cluster setting ts.blocks_per_segment.max_limit=50;

CREATE TS DATABASE tsdb;
CREATE TABLE tsdb.t1(
ts TIMESTAMPTZ NOT NULL,e1 TIMESTAMP,e2 INT2,e3 INT4,e4 INT8,e5 FLOAT4,e6 FLOAT8,e7 BOOL,e8 CHAR,e10 NCHAR,e16 VARBYTES
) TAGS (
tag1 BOOL,tag2 SMALLINT,tag3 INT,tag4 BIGINT,tag5 FLOAT4,tag6 DOUBLE,tag7 VARBYTES,tag11 CHAR,tag13 NCHAR NOT NULL
)PRIMARY TAGS(tag13);

-- drop
DROP TABLE tsdb.t1;

