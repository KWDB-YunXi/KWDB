// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

#include <windows.h>
#include <sql.h>
#include <sqlext.h>
#include <stdio.h>

#define BUFSIZE 1024

typedef struct
{
    SQLSMALLINT SqlType;
    SQLULEN ParamSize;
    SQLSMALLINT DecimalDigits;

} MyParamDesc;

int main()
{
    SQLHENV envHandle;
    SQLHDBC connHandle;
    SQLHSTMT stmtHandle;
    SQLRETURN retcode;

    // alloc handle
    retcode = SQLAllocHandle(SQL_HANDLE_ENV, SQL_NULL_HANDLE, &envHandle);
    if (SQL_SUCCESS != retcode)
    {
        // handle error
        return 1;
    }

    // set odbc version 3.0
    retcode = SQLSetEnvAttr(envHandle, SQL_ATTR_ODBC_VERSION, (SQLPOINTER)SQL_OV_ODBC3, 0);
    if (SQL_SUCCESS != retcode)
    {
        // handle error
        return 1;
    }

    // alloc conn handle
    retcode = SQLAllocHandle(SQL_HANDLE_DBC, envHandle, &connHandle);
    if (SQL_SUCCESS != retcode)
    {
        // handle error
        return 1;
    }

    // connect database
    // Construct the connection string
    SQLWCHAR connStrIn[] = L"DSN=PostgreSQL35W;UID=root;PWD=123456";

    // Connect to the database
    retcode = SQLDriverConnect(connHandle, NULL, connStrIn, SQL_NTS, NULL, 0, NULL, SQL_DRIVER_COMPLETE);
    if (SQL_SUCCESS != retcode && SQL_SUCCESS_WITH_INFO != retcode)
    {
        // handle error
        return 1;
    }

    // alloc handle
    retcode = SQLAllocHandle(SQL_HANDLE_STMT, connHandle, &stmtHandle);
    if (SQL_SUCCESS != retcode)
    {
        // handle error
        printf("Failed to allocate statement handle: %d\n", retcode);
        return 1;
    }

    MyParamDesc paramDescArray[1];
    paramDescArray[0].SqlType = SQL_INTEGER;
    paramDescArray[0].ParamSize = 0;
    paramDescArray[0].DecimalDigits = 0;

    // set SQL_ATTR_APP_PARAM_DESC
    retcode = SQLSetStmtAttr(stmtHandle, SQL_ATTR_APP_PARAM_DESC, (SQLPOINTER)paramDescArray, SQL_IS_POINTER);
    if (retcode == SQL_SUCCESS)
    {
        printf("SQL_ATTR_APP_PARAM_DESC set successfully\n");
    }
    else if (retcode == SQL_SUCCESS_WITH_INFO)
    {
        printf("SQL_ATTR_APP_PARAM_DESC set with information\n");
    }
    else
    {
        printf("SQLSetStmtAttr(SQL_ATTR_APP_PARAM_DESC)\n");
    }

    SQLFreeHandle(SQL_HANDLE_STMT, stmtHandle);
    // clear
    SQLDisconnect(connHandle);
    SQLFreeHandle(SQL_HANDLE_DBC, connHandle);
    SQLFreeHandle(SQL_HANDLE_ENV, envHandle);

    return 0;
}
