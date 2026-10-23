# 按需触发的邮件接收

## 选型

保留现有 Go + PostgreSQL + MIME 文件归档，不增加 Node.js、Redis 或另一个邮件平台。
参考的是成熟项目的协议与状态管理设计，并非整体部署或拷贝它们的实现：

- [ImapFlow](https://github.com/postalsys/imapflow)：MIT 许可，支持 IDLE、邮箱锁和流式读取，以及按服务器能力启用的 CONDSTORE/QRESYNC 等扩展。
- [go-imap](https://github.com/emersion/go-imap)：本项目已使用的 Go IMAP 协议库；IDLE 命令自动约每 28 分钟续订，避免长期空闲超时。
- [EmailEngine](https://github.com/postalsys/emailengine)：参考通知唤醒与持久化同步状态的分工。其当前许可为商业源码可用许可，项目没有将它当作免费开源依赖引入。

默认 `ICLOUD_API_MAIL_ON_DEMAND_ONLY=true` 且启用 `ICLOUD_API_IMAP_IDLE_ENABLED`：服务按活跃主号建立共享 receiver，不在启动时执行全量收件，也不做无条件周期轮询。主号连续 10 分钟没有有效取件后 receiver 休眠；每个主号最多保持 1 条 IDLE 通知连接和 1 条串行 fetch 连接，manager 的全局 `sync_concurrency` 不变。收到请求且上次观察超过 5 秒时执行共享补查，错误退避带抖动。真实 Apple 联测存在连接仍在但未及时通知新信的情况，因此连接正常和断线状态均采用此观察期限；5 秒不是 Apple 投递或端到端延迟保证，连接、排队和下载仍需时间。`ICLOUD_API_IMAP_IDLE_ENABLED=false` 时回退到单 alias 按需路径；webmail 继续使用原有 callback。`ICLOUD_API_MAIL_ON_DEMAND_ONLY=false` 保留历史兼容行为。

## 上游资源与一致性

1. OTP Bearer、`?token=` 直链、legacy latest/recent 和 pool lease code，以及浏览器直接访问/刷新，复用相同收件工作器。接口格式不变，root 与其合法 `+tag` 聚合；分裂和任务串行由调用项目自行管理。
2. 共享模式下同一 root 的所有取件凭据共用容量 2 的令牌桶，每 2 秒恢复 1 次额度，最多积累 2 次。额度从请求准入时扣除，等待过程中持续恢复，结束、失败或取消不追加完成后冷却；被拦截请求不改恢复时刻。额度内的同 root 并发读者共享同步，普通读取和 Pool 长等待不互斥；各自完成仅释放自己的计数。主号实际 IMAP 获取起点仍最短间隔 2 秒，由共享 receiver 合并，HTTP 突发额度不等于额外 Apple 建连或读取。
3. 活跃 receiver 最多服务 128 个主号；共享 HTTP 取件移除每主号 64 和全站每秒 100 次限制，允许全站 512 个在途、入口总计最多 1024 个请求（含排队），排队预算 2 秒。Pool 长等待最多 384 个在途、总计最多 640 个（含排队），为普通读取预留执行和入口容量。并发、队满、排队超时与 alias 状态表容量不足返回 `503 PICKUP_BUSY`；429 仅表示刷新速率超限。`Retry-After` 为建议复查秒数，`X-Retry-After-Ms` 为毫秒值，`X-RateLimit-Reason` 区分原因，不承诺到时任务已结束。旧非共享路径保留全站 16、主号 2、同 alias 独占和完成后 2 秒冷却，容量不足同样返回 503；其每秒 100 次入口速率限制仍返回 429。后台与健康检查不占这些名额。
4. 首次同步、UIDVALIDITY 变化或游标超过 15 分钟时，使用服务端日期筛选定位最近时间窗口，再按 UID 分页；不再把恢复窗口截断为最后 128 封。SINCE 仅精确到日期，向前放宽一天覆盖时区，正文通过 INTERNALDATE 精确过滤最近 15 分钟；支持 ESEARCH 的服务器仅返回最小 UID 与数量，旧服务器返回日期匹配的 UID 元数据，不下载历史正文。128 是生产入口的单批处理量，库默认值为 32；超过单批数量继续持久化游标。待处理时间边界存入 `imap_active_windows`，与归档、游标同事务提交，跨批或重启不重新裁剪，全部处理完清除。窗口开始前的历史不作完整归档保证；旧单 alias 游标保留用于兼容路径。
5. 上游先批量读取新增 UID 头部，严格核验完整地址优先、首个 + 后标签回根且域名不变，仅命中已登记 alias 才 FETCH 正文；public IMAPS 仅读本地归档。共享工作器不下载未命中地址的正文。
6. 正文使用 PEEK 流式归档，事务提交归档与游标；未提交批次不推进游标。保留 TLS 验证，不记录密码或原始 IMAP 报文。
7. 旧版 recent 入口的消费语义保持不变：消费后通过原有队列回写上游已读标记，失败任务保留重试；这类任务不搜索或下载新邮件。隐藏邮箱创建计划与邮件收取独立。
8. 同步中的通知保留到下一批；读者等待入场时的观察代次，持续来信不会让请求无限等待。断线、提交失败返回可重试结果，不把失败当成已同步；凭据变化或停用时取消旧工作器。客户端断开只取消自己的等待，服务停机则等待共享工作器退出。
9. Pool lease code 仅使用上述专用取件预算，不再重复消耗 Pool 通用的每 IP 每分钟 300 次额度，避免多个 root 合法轮询互相挤占。Pool Key 鉴权、租约归属及状态校验保持不变；其他 Pool 路由保留通用额度，超限携带 `Retry-After: 60`。

网页按需收件保存的是正文重建版，IMAP 保存完整 MIME，二者可对应同一主号、UIDVALIDITY 和 UID。切换到 IMAP 时，仅在非空 Message-ID 一致、旧文件大小及校验和通过、旧正文确有本项目网页重建标记时升级；完整原文先写独立文件，事务提交后再清理被替代的重建版。失败或观察已过期时保留旧文件与数据库记录。升级不改变归档 ID、邮箱本地 UID 或既有取码记录；后续网页读取不覆盖完整原文。普通原文内容冲突、身份不一致及损坏文件继续报错，不能通过覆盖文件绕过校验。

IMAP 增量同步每批最多处理既定数量的 UID，并将这一批需要归档的完整 MIME 合并为一次 UID FETCH；提交仍按批次原子推进，单批失败不会前移游标。对不支持批量 BODY FETCH 的服务器保留逐封回退，正文大小上限、超大邮件跳过正文和临时文件清理规则不变。Pool 取码的本地复验使用共享读锁，不再串行所有读者；轮换、释放和退役仍使用写锁。Pool 等待预算在首次同步阶段用完时，同样执行最终本地读取，返回已有新码或正常 `no_code`，客户端取消则停止自己的请求。

## 同主号退役与收件并行

Apple 管理与 IMAP 读取使用独立的主号执行锁。Apple 操作继续按主号串行，IMAP 获取继续按主号串行并占用既有全局连接预算；两类网络操作可以重叠，不因同主号正在退役其他邮箱而阻塞收件。自动、手动、共享 receiver 与单 alias 按需路径都沿用这一边界。

旧 recent 的上游已读回写属于远端修改，继续取得管理锁以固定邮箱来源，再取得 IMAP 执行名额；等待管理锁时不占收件连接槽。这一保留边界不用于普通 OTP/Pool 收件。

邮件发布在本地事务中锁定账户行，核验从同一次账户读取取得的邮箱配置与通道版本，逐项重新核验观察过的 alias 身份。只移除已删除、停用或期间改变的 alias 关联，不丢弃其他地址的整批归档；完全失去有效归属的正文不入库并清理暂存文件。新加入但未观察到的 alias 不标记同步成功。游标仍与保留的归档一起原子提交，不回退。

主号配置版本复用已有 mailbox settings 与 mail transport 的持久化修订，区别于随 alias 增删而变化的账户 updated_at。alias 使用新增的 `alias_receive_revisions` 表及启停触发器；SQLite/PostgreSQL 迁移一致、幂等，旧 alias 默认修订 0，无需回填。最近访问、备注、分组或单邮箱凭据轮换不会误丢新邮件，启停后切回仍会排除旧观察。密码、来源、主号启停与通道变化使旧网络观察失效，纯主号名称变化不使收件失效。失败状态也按同一配置观察核验，避免旧密码失败覆盖新配置。

Pool 接受退役时仍立即关闭 retiring lease 的取码、续期、释放与再次领取。未知 Apple 删除结果继续 review / interrupted，不自动重放；远端确认和本地退休收口语义不变。旧管理批量删除、严格完整地址优先与同域 root+tag 归属保持原合同。

离线 TLS IMAP 集成用阻塞 BODY.PEEK 的 fixture 验证退役可在真实协议读取未完成时收口，随后 survivor 的 +tag 邮件正常归档；同时覆盖完整 + 地址优先、未知 root / 不同域名跳过，以及 retiring / retired Pool 取码关闭。公开文档由 `internal/httpserver/docs.html` 内置提供，修改 Markdown 本身不会更新 `/docs/`，需重新构建网关。

## 控制面板

- 普通管理页面空闲时每 30 秒更新一次，正在同步/批量删除时使用 5 秒周期。
- 隐藏标签页或浏览器离线时停止自动请求，回到前台/重新联网时刷新；同一刷新器只允许一个请求在途。
- 请求失败按指数退避至 120 秒，成功恢复普通周期。日志页保持其显式启用的 5 秒刷新。
- 管理页面刷新读取本地数据，不等价于上游 IMAP 读取。正在读取邮件仅对应真实同步任务，不用伪造完成状态来掩盖网络请求。
- 服务端依据 Vite manifest，在首屏 HTML 中仅预载目标页面的本地 JS/CSS 依赖，减少入口脚本到页面脚本的串行发现；不预取其他页面或邮箱 API。没有 manifest 的旧前端产物继续兼容。

## 验证

```sh
flock .local/project-heavy.lock env GOMAXPROCS=2 GOMEMLIMIT=512MiB go test -p 1 -parallel 2 ./internal/mail ./internal/store ./internal/syncer ./internal/httpserver -run 'TestFetchActive|TestFetchAliasIncremental|TestAliasMailboxSync|TestAliasDemand|TestPoolLeaseCodeDemand|TestActiveReceiver' -count=1
```

上线后分别记录冷启动、热缓存、不同 root 并发与新信通知后的取件耗时；检查实际 IMAP 登录/正文读取数量，而非只检查 HTTP 200。

取件预算的时序、错误原因和客户端建议见 [分层取件限流](PICKUP-RATE.md)。
