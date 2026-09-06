# 入力は Aurora / RDS モジュールと揃える(仕様17章)。
# 「RDS を選ぶところで sashiki を選べる」ため、ラッパーモジュールが
# engine=aurora|rds-mysql|sashiki を同じ変数で切り替えられるようにする。

variable "name" {
  description = "リソース名のプレフィックス(RDS の identifier 相当)"
  type        = string
}

variable "vpc_id" {
  description = "配置先 VPC"
  type        = string
}

variable "subnet_ids" {
  description = "配置先サブネット(EC2 は subnet_ids[0] に置く。list を受けるのは RDS モジュール互換のため)"
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) > 0
    error_message = "subnet_ids には最低1つのサブネットが必要です。"
  }
}

variable "allowed_sg_ids" {
  description = "DB(3306)/ API(8080)への接続を許可する security group の一覧"
  type        = list(string)
  default     = []
}

variable "instance_class" {
  description = "EC2 インスタンスタイプ(RDS の instance_class 相当)。変更前に `sashiki drain` で全ブランチを停止すること"
  type        = string
  default     = "m6i.large"
}

variable "allocated_storage" {
  description = "データ用 EBS のサイズ(GiB)。ZFS プールになる"
  type        = number
  default     = 100
}

variable "engine_version" {
  description = "MySQL のメジャーバージョン(user-data のパッケージ選択に使う)"
  type        = string
  default     = "8.0"
}

variable "ami_id" {
  description = "ベース AMI。空なら最新の Ubuntu 24.04 LTS(amd64)を自動解決する"
  type        = string
  default     = ""
}

variable "ebs_type" {
  description = "データ EBS のボリュームタイプ"
  type        = string
  default     = "gp3"
}

variable "data_device_name" {
  description = "データ EBS のデバイス名(user-data の zpool 作成に渡す)"
  type        = string
  default     = "/dev/xvdf"
}

variable "route53_zone_id" {
  description = "エンドポイント用の Route53 ホストゾーン ID。空なら DNS レコードを作らず private IP を endpoint に返す"
  type        = string
  default     = ""
}

variable "dns_name" {
  description = "エンドポイントの FQDN(route53_zone_id 指定時に A レコードを作る)。例 db.internal.example.com"
  type        = string
  default     = ""
}

variable "key_name" {
  description = "SSH キーペア名(空なら SSH 鍵を紐付けない)"
  type        = string
  default     = ""
}

variable "proxy_user" {
  description = "ブランチ接続用のプロキシユーザー名(branch_user 出力に使う)"
  type        = string
  default     = "dev"
}

variable "github_token" {
  description = "install.sh が private リポジトリから deb を取得するための token(public 化後は不要)"
  type        = string
  default     = ""
  sensitive   = true
}

variable "sashiki_ref" {
  description = "install.sh / モジュールが参照する sashiki のリリースタグ(バイナリとモジュールを同じ ref で固定する)"
  type        = string
  default     = "latest"
}

variable "tags" {
  description = "全リソースに付与するタグ"
  type        = map(string)
  default     = {}
}
