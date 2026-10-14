# llm-api 数据迁移指南（旧机器 → 本机）

数据位置：MongoDB 的 `llm_api` 库（skillbook 的 mongo 容器内），
包含集合：tokens、virtualModels、quotas、channelKeys、requestLogs、settings。

迁移路径：旧机器导出 archive 压缩包 → U 盘/网盘中转 → 本机导入。

---

## 第 1 步：旧机器上导出数据

在旧机器的 llm-api 项目目录（或任意位置）执行：

```bash
# 确认 mongo 容器名（应为 skillbook-mongo-1，如不同请替换下文命令中的名称）
docker ps --format '{{.Names}}'

# 导出整个 llm_api 库（gzip 压缩的 archive 格式）
docker exec skillbook-mongo-1 mongodump \
  --uri="mongodb://localhost:27017" \
  --db=llm_api \
  --archive --gzip \
  > llm_api_$(date +%Y%m%d).archive.gz

# 校验文件完整性和内容
gzip -t llm_api_$(date +%Y%m%d).archive.gz && echo "gzip 完整性 OK"
docker exec skillbook-mongo-1 mongorestore --uri="mongodb://localhost:27017" \
  --archive --gzip --dryRun < llm_api_$(date +%Y%m%d).archive.gz
```

## 第 2 步：拷贝到中转介质

把生成的 `llm_api_YYYYMMDD.archive.gz` 拷贝到 U 盘/网盘，
再下载到本机任意位置，例如 `/tmp/opencode/llm_api_YYYYMMDD.archive.gz`。
注意文件大小，确保传输过程中没有截断（可在本机再次 `gzip -t` 校验）。

## 第 3 步：本机导入

在本机执行（把文件路径替换成实际路径）：

```bash
cd /usr/local/code/llm-api

# 停掉服务，避免导入期间写冲突（可选但推荐）
docker compose stop llm-router

# 导入（--drop 会先删除本机同名集合再导入，实现整库覆盖）
docker exec -i skillbook-mongo-1 mongorestore \
  --uri="mongodb://localhost:27017" \
  --archive --gzip --drop \
  < /tmp/opencode/llm_api_YYYYMMDD.archive.gz

# 恢复服务
docker compose start llm-router
```

## 第 4 步：验证

```bash
# 各集合文档数应与旧机器一致
docker exec skillbook-mongo-1 mongosh --quiet --eval '
  db.getSiblingDB("llm_api").getCollectionNames().forEach(c => {
    print(c + ": " + db.getSiblingDB("llm_api")[c].countDocuments() + " docs");
  })'

# 服务健康检查（返回 200 即正常）
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8081/console/
```

浏览器访问 http://localhost:8081/console/ 用旧机器的管理员账号登录验证。

---

## 注意事项

1. **settings 会被覆盖**：本机原有的 3 条 settings（含控制台显示的配置）
   会被旧机器的数据覆盖，符合"整库迁移"的预期。
2. **.env 不随数据迁移**：`.env`（ADMIN_USER / ADMIN_PASSWORD 等）
   是本机配置文件，不会被导入。如果旧机器的 `.env` 与本机不同，
   请手动核对，必要时修改本机 `.env` 后执行 `./restart.sh`。
3. **requestLogs 可能很大**：如果导出文件特别大且不需要历史日志，
   可在导出时排除日志集合：
   `--excludeCollection=requestLogs`（导入后本机该集合为空）。
