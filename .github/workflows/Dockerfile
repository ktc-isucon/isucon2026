FROM ubuntu:22.04

# 必要なパッケージをインストール
RUN apt-get update && apt-get install -y openssh-server sudo curl git

# # SSHの設定
RUN mkdir -p /run/sshd

# isuconユーザーの作成とsudo設定
RUN useradd -m -s /bin/bash isucon && adduser isucon sudo
RUN echo "isucon ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/isucon

USER root

# SSHポートを公開
EXPOSE 22

# SSHDをフォアグラウンドで実行
CMD ["/usr/sbin/sshd", "-D"]
