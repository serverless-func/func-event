# func-event

定时脚本执行结果上报服务。HTTP 上报 + SQLite 存储，每个事件保留最近 N 条记录。

## 启动

```bash
go build -o app .
EVENT_TOKEN=your-token EVENT_DB_PATH=./events.db FC_SERVER_PORT=9000 ./app
```

Docker：

```bash
docker build -t func-event .
docker run -d -p 9000:9000 -v $PWD/data:/data \
  -e EVENT_TOKEN=your-token -e EVENT_DB_PATH=/data/events.db func-event
```

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `EVENT_TOKEN` | 是 | - | 接口 Token，未配置则拒绝启动 |
| `EVENT_DB_PATH` | 否 | `events.db` | SQLite 文件路径，启动时自动建表 |
| `EVENT_DAYS` | 否 | `90` | 每个事件保留的最近记录条数 |
| `FC_SERVER_PORT` | 否 | `9000` | 监听端口 |

## 接口

上报和查询都要带 `Authorization: Bearer <TOKEN>`，失败返回 401。

### 成功上报

```bash
curl -X POST http://127.0.0.1:9000/report \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{"id":"home-backup","name":"家庭服务器备份","success":true,"message":"已备份 3 个目录"}'
# {"status":"ok"}
```

### 失败上报

```bash
curl -X POST http://127.0.0.1:9000/report \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{"id":"home-backup","name":"家庭服务器备份","success":false,"message":"已备份 3 个目录","error":"上传远端失败"}'
```

### 查询最新结果

```bash
curl -H "Authorization: Bearer your-token" http://127.0.0.1:9000/latest
```

```json
[
  {
    "id": "home-backup",
    "name": "家庭服务器备份",
    "success": false,
    "message": "已备份 3 个目录",
    "error": "上传远端失败",
    "reportedAt": "2026-11-02T02:58:00.070543902Z"
  }
]
```

字段说明：`id`、`name`、`success` 必填；`message`、`error` 可选；`reportedAt` 由服务端生成，UTC / RFC 3339。

请求体上限 64 KiB，超限返回 413；参数校验失败返回 400。`GET /ping` 为健康检查，无需 Token。

## 数据库

单文件 SQLite，journal 模式固定为 `DELETE`（不使用 WAL），断开连接后不残留 `-wal` / `-shm` 文件，备份直接复制 `.db` 即可。
