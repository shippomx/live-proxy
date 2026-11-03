# live-proxy 运行镜像。
#
# 二进制在镜像外交叉编译好再放进来（见 deploy-docker.sh），而不是多阶段构建里跑 go build：
# 多阶段需要在构建机上拉 golang 基础镜像，路由器上拉 Docker Hub 在国内经常失败，
# 而 scratch 不需要拉任何东西，路由器本地就能 build。
#
# 构建上下文里需要两个文件：
#   live-proxy               linux 静态二进制（CGO_ENABLED=0）
#   ca-certificates.crt      CA 证书包 —— scratch 里没有，缺了所有 HTTPS 取流都会
#                            报 x509 错误，而日志里只显示"上游接口请求失败"
FROM scratch

COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY live-proxy /live-proxy

# 时区数据已嵌入二进制（time/tzdata），这里只需指定用哪个；不设日志会是 UTC，差 8 小时。
ENV TZ=Asia/Shanghai

# 只需要监听非特权端口和发起出站请求，不需要 root。
USER 65534:65534

EXPOSE 9000
ENTRYPOINT ["/live-proxy"]
CMD ["-addr", ":9000"]
