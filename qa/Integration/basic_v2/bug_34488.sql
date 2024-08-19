create ts database test;
use test;

CREATE TABLE t_cnc (k_timestamp TIMESTAMPTZ NOT NULL,cnc_sn VARCHAR(200) NULL,cnc_sw_mver VARCHAR(30) NULL,cnc_sw_sver VARCHAR(30) NULL,cnc_tol_mem VARCHAR(10) NULL,cnc_use_mem VARCHAR(10) NULL,cnc_unuse_mem VARCHAR(10) NULL,cnc_status VARCHAR(2) NULL,path_quantity VARCHAR(30) NULL,axis_quantity VARCHAR(30) NULL,axis_path VARCHAR(100) NULL,axis_type VARCHAR(100) NULL,axis_unit VARCHAR(100) NULL,axis_num VARCHAR(100) NULL,axis_name VARCHAR(100) NULL,sp_name VARCHAR(100) NULL,abs_pos VARCHAR(200) NULL,rel_pos VARCHAR(200) NULL,mach_pos VARCHAR(200) NULL,dist_pos VARCHAR(200) NULL,sp_override FLOAT8 NULL,sp_set_speed VARCHAR(30) NULL,sp_act_speed VARCHAR(30) NULL,sp_load VARCHAR(300) NULL,feed_set_speed VARCHAR(30) NULL,feed_act_speed VARCHAR(30) NULL,feed_override VARCHAR(30) NULL,servo_load VARCHAR(300) NULL,parts_count VARCHAR(30) NULL,cnc_cycletime VARCHAR(30) NULL,cnc_alivetime VARCHAR(30) NULL,cnc_cuttime VARCHAR(30) NULL,cnc_runtime VARCHAR(30) NULL,mprog_name VARCHAR(500) NULL,mprog_num VARCHAR(30) NULL,sprog_name VARCHAR(500) NULL,sprog_num VARCHAR(30) NULL,prog_seq_num VARCHAR(30) NULL,prog_seq_content VARCHAR(1000) NULL,alarm_count VARCHAR(10) NULL,alarm_type VARCHAR(100) NULL,alarm_code VARCHAR(100) NULL,alarm_content VARCHAR(2000) NULL,alarm_time VARCHAR(200) NULL,cur_tool_num VARCHAR(20) NULL,cur_tool_len_num VARCHAR(20) NULL,cur_tool_len VARCHAR(20) NULL,cur_tool_len_val VARCHAR(20) NULL,cur_tool_x_len VARCHAR(20) NULL,cur_tool_x_len_val VARCHAR(20) NULL,cur_tool_y_len VARCHAR(20) NULL,cur_tool_y_len_val VARCHAR(20) NULL,cur_tool_z_len VARCHAR(20) NULL,cur_tool_z_len_val VARCHAR(20) NULL,cur_tool_rad_num VARCHAR(20) NULL,cur_tool_rad VARCHAR(20) NULL,cur_tool_rad_val VARCHAR(20) NULL,device_state INT4 NULL,value1 VARCHAR(10) NULL,value2 VARCHAR(10) NULL,value3 VARCHAR(10) NULL,value4 VARCHAR(10) NULL,value5 VARCHAR(10) NULL) TAGS (machine_code VARCHAR(64) NOT NULL,op_group VARCHAR(64) NOT NULL,brand VARCHAR(64) NOT NULL,number_of_molds INT4 ) PRIMARY TAGS(machine_code, op_group);

CREATE TABLE t_electmeter (k_timestamp TIMESTAMPTZ NOT NULL,elect_name VARCHAR(63) NOT NULL,vol_a FLOAT8 NOT NULL,cur_a FLOAT8 NOT NULL,powerf_a FLOAT8 NULL,allenergy_a INT4 NOT NULL,pallenergy_a INT4 NOT NULL,rallenergy_a INT4 NOT NULL,allrenergy1_a INT4 NOT NULL,allrenergy2_a INT4 NOT NULL,powera_a FLOAT8 NOT NULL,powerr_a FLOAT8 NOT NULL,powerl_a FLOAT8 NOT NULL,vol_b FLOAT8 NOT NULL,cur_b FLOAT8 NOT NULL,powerf_b FLOAT8 NOT NULL,allenergy_b INT4 NOT NULL,pallenergy_b INT4 NOT NULL,rallenergy_b INT4 NOT NULL,allrenergy1_b INT4 NOT NULL,allrenergy2_b INT4 NOT NULL,powera_b FLOAT8 NOT NULL,powerr_b FLOAT8 NOT NULL,powerl_b FLOAT8 NOT NULL,vol_c FLOAT8 NOT NULL,cur_c FLOAT8 NOT NULL,powerf_c FLOAT8 NOT NULL,allenergy_c INT4 NOT NULL,pallenergy_c INT4 NOT NULL,rallenergy_c INT4 NOT NULL,allrenergy1_c INT4 NOT NULL,allrenergy2_c INT4 NOT NULL,powera_c FLOAT8 NOT NULL,powerr_c FLOAT8 NOT NULL,powerl_c FLOAT8 NOT NULL,vol_ab FLOAT8 NULL,vol_bc FLOAT8 NULL,vol_ca FLOAT8 NULL,infre FLOAT8 NOT NULL,powerf FLOAT8 NOT NULL,allpower FLOAT8 NOT NULL,pallpower FLOAT8 NOT NULL,rallpower FLOAT8 NOT NULL,powerr FLOAT8 NOT NULL,powerl FLOAT8 NOT NULL,allrenergy1 FLOAT8 NOT NULL,allrenergy2 FLOAT8 NOT NULL) TAGS (machine_code VARCHAR(64) NOT NULL,op_group VARCHAR(64) NOT NULL,location VARCHAR(64) NOT NULL,cnc_number INT4 ) PRIMARY TAGS(machine_code);

explain select  
  41 as c0, 
  subq_0.c0 as c1, 
  subq_0.c4 as c2, 
  subq_0.c3 as c3
from 
  (select  
        ref_0.allrenergy1_a as c0, 
        ref_0.powerr as c1, 
        ref_0.allpower as c2, 
        3 as c3, 
        ref_0.vol_b as c4, 
        ref_0.vol_ca as c5
      from 
        public.t_electmeter as ref_0
      where ref_0.powerf is not NULL
      limit 44) as subq_0
where subq_0.c2 is NULL
limit 143;


drop database test cascade;