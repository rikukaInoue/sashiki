#!/usr/bin/env bash
# FSx for OpenZFS バックエンドの実 AWS E2E。ローカル(mac)から実行する。
#   ./e2e/fsx/run.sh
# 前提: AWS 認証済み / キーペア db-branch-poc と SSH 用 SG db-branch-poc が存在
# 費用: FSx(6円/h) + EC2 t3.small で 1 回 40 円前後。終了時に全リソースを削除する。
set -euo pipefail

REGION=ap-northeast-1
KEY=~/.ssh/db-branch-poc.pem
SSH_SG=$(aws ec2 describe-security-groups --region $REGION \
  --filters Name=group-name,Values=db-branch-poc --query 'SecurityGroups[0].GroupId' --output text)
VPC=$(aws ec2 describe-vpcs --region $REGION --filters Name=is-default,Values=true \
  --query 'Vpcs[0].VpcId' --output text)
SUBNET=$(aws ec2 describe-subnets --region $REGION \
  --filters Name=vpc-id,Values=$VPC Name=availability-zone,Values=${REGION}a \
  --query 'Subnets[0].SubnetId' --output text)
ROOT=$(cd "$(dirname "$0")/../.." && pwd)

log() { echo -e "\n=== $* ==="; }
CLEANUP=()
cleanup() {
  log "teardown"
  # 逆順で実行(SG は FSx/EC2 が消えて ENI が外れてから)
  for ((i=${#CLEANUP[@]}-1; i>=0; i--)); do eval "${CLEANUP[$i]}" || true; done
}
trap cleanup EXIT

# --- 1. FSx SG + filesystem(作成に 5〜8 分かかるので最初に投げる) ---
log "create fsx filesystem"
FSXSG=$(aws ec2 create-security-group --region $REGION --group-name twig-fsx-e2e \
  --description "twig fsx e2e" --vpc-id $VPC --query 'GroupId' --output text)
CLEANUP+=("aws ec2 delete-security-group --region $REGION --group-id $FSXSG")
aws ec2 authorize-security-group-ingress --region $REGION --group-id $FSXSG --ip-permissions "[
  {\"IpProtocol\":\"tcp\",\"FromPort\":111,\"ToPort\":111,\"UserIdGroupPairs\":[{\"GroupId\":\"$SSH_SG\"}]},
  {\"IpProtocol\":\"udp\",\"FromPort\":111,\"ToPort\":111,\"UserIdGroupPairs\":[{\"GroupId\":\"$SSH_SG\"}]},
  {\"IpProtocol\":\"tcp\",\"FromPort\":2049,\"ToPort\":2049,\"UserIdGroupPairs\":[{\"GroupId\":\"$SSH_SG\"}]},
  {\"IpProtocol\":\"udp\",\"FromPort\":2049,\"ToPort\":2049,\"UserIdGroupPairs\":[{\"GroupId\":\"$SSH_SG\"}]},
  {\"IpProtocol\":\"tcp\",\"FromPort\":20001,\"ToPort\":20003,\"UserIdGroupPairs\":[{\"GroupId\":\"$SSH_SG\"}]},
  {\"IpProtocol\":\"udp\",\"FromPort\":20001,\"ToPort\":20003,\"UserIdGroupPairs\":[{\"GroupId\":\"$SSH_SG\"}]}
]" > /dev/null

FSID=$(aws fsx create-file-system --region $REGION \
  --file-system-type OPENZFS --storage-capacity 64 --storage-type SSD \
  --subnet-ids $SUBNET --security-group-ids $FSXSG \
  --open-zfs-configuration 'DeploymentType=SINGLE_AZ_1,ThroughputCapacity=64,AutomaticBackupRetentionDays=0,RootVolumeConfiguration={RecordSizeKiB=16,DataCompressionType=LZ4}' \
  --tags Key=Name,Value=twig-fsx-e2e --query 'FileSystem.FileSystemId' --output text)
CLEANUP+=("aws fsx delete-file-system --region $REGION --file-system-id $FSID --open-zfs-configuration 'SkipFinalBackup=true,Options=[DELETE_CHILD_VOLUMES_AND_SNAPSHOTS]'; while aws fsx describe-file-systems --region $REGION --file-system-ids $FSID > /dev/null 2>&1; do sleep 15; done")
echo "filesystem: $FSID"

# --- 2. EC2(FSx 作成待ちの間に用意) ---
log "launch ec2"
AMI=$(aws ssm get-parameter --region $REGION \
  --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id \
  --query 'Parameter.Value' --output text)
IID=$(aws ec2 run-instances --region $REGION --image-id $AMI --instance-type t3.small \
  --key-name db-branch-poc --security-group-ids $SSH_SG \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=twig-fsx-e2e}]' \
  --query 'Instances[0].InstanceId' --output text)
CLEANUP+=("aws ec2 terminate-instances --region $REGION --instance-ids $IID > /dev/null; aws ec2 wait instance-terminated --region $REGION --instance-ids $IID")
aws ec2 wait instance-running --region $REGION --instance-ids $IID
IP=$(aws ec2 describe-instances --region $REGION --instance-ids $IID \
  --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
echo "ec2: $IID $IP"
SSH="ssh -i $KEY -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 ubuntu@$IP"

log "provision ec2 (packages)"
for _ in $(seq 1 20); do $SSH true 2>/dev/null && break; sleep 10; done
$SSH 'sudo bash -s' <<'EOS'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -q > /dev/null
apt-get install -y -q mysql-server-8.0 mysql-client-8.0 nfs-common apparmor-utils > /dev/null 2>&1
systemctl stop mysql; systemctl disable mysql 2>/dev/null
if [ -f /etc/apparmor.d/usr.sbin.mysqld ]; then
  mkdir -p /etc/apparmor.d/disable
  ln -sf /etc/apparmor.d/usr.sbin.mysqld /etc/apparmor.d/disable/ || true
  apparmor_parser -R /etc/apparmor.d/usr.sbin.mysqld 2>/dev/null || true
fi
mkdir -p /etc/twig/hooks /var/lib/twig/branches /var/log/twig/hooks /mnt/twig
EOS

log "install twig binaries + systemd unit"
GOOS=linux GOARCH=amd64 go build -o /tmp/twigd-fsx "$ROOT/cmd/twigd"
GOOS=linux GOARCH=amd64 go build -o /tmp/twig-fsx "$ROOT/cmd/twig"
scp -q -i $KEY /tmp/twigd-fsx ubuntu@$IP:/tmp/twigd
scp -q -i $KEY /tmp/twig-fsx ubuntu@$IP:/tmp/twig
scp -q -i $KEY "$ROOT/deploy/systemd/mysqld@.service" ubuntu@$IP:/tmp/
$SSH 'sudo install -m755 /tmp/twigd /usr/local/bin/twigd && sudo install -m755 /tmp/twig /usr/local/bin/twig && sudo cp /tmp/mysqld@.service /etc/systemd/system/ && sudo sed -i "s|/var/log/twig/%i.err|/var/log/twig/%i.err --innodb-buffer-pool-size=128M|; s|--innodb-buffer-pool-size=256M||" /etc/systemd/system/mysqld@.service && sudo systemctl daemon-reload'

# --- 3. FSx AVAILABLE 待ち → base ボリューム作成 ---
log "wait fsx available"
while [ "$(aws fsx describe-file-systems --region $REGION --file-system-ids $FSID --query 'FileSystems[0].Lifecycle' --output text)" != "AVAILABLE" ]; do sleep 20; done
DNS=$(aws fsx describe-file-systems --region $REGION --file-system-ids $FSID --query 'FileSystems[0].DNSName' --output text)
ROOTVOL=$(aws fsx describe-file-systems --region $REGION --file-system-ids $FSID --query 'FileSystems[0].OpenZFSConfiguration.RootVolumeId' --output text)

log "create base volume + baseline"
BASEVOL=$(aws fsx create-volume --region $REGION --volume-type OPENZFS --name base \
  --open-zfs-configuration "ParentVolumeId=$ROOTVOL,RecordSizeKiB=16,DataCompressionType=LZ4,NfsExports=[{ClientConfigurations=[{Clients=*,Options=[rw,crossmnt,no_root_squash]}]}]" \
  --query 'Volume.VolumeId' --output text)
while [ "$(aws fsx describe-volumes --region $REGION --volume-ids $BASEVOL --query 'Volumes[0].Lifecycle' --output text)" != "AVAILABLE" ]; do sleep 5; done

$SSH "sudo bash -s" <<EOS
set -euo pipefail
mkdir -p /mnt/twig-base
mount -t nfs -o nfsvers=4.1 ${DNS}:/fsx/base /mnt/twig-base
mkdir -p /mnt/twig-base/data /var/log/twig
chown -R mysql:mysql /mnt/twig-base /var/log/twig
sudo -u mysql mysqld --initialize-insecure --datadir=/mnt/twig-base/data > /dev/null 2>&1
sudo -u mysql mysqld --datadir=/mnt/twig-base/data --skip-networking \
  --socket=/tmp/base.sock --pid-file=/tmp/base.pid --log-error=/var/log/twig/base.err --daemonize
for _ in \$(seq 1 60); do mysqladmin -uroot -S /tmp/base.sock ping > /dev/null 2>&1 && break; sleep 1; done
mysql -uroot -S /tmp/base.sock <<'SQL'
CREATE DATABASE app;
CREATE TABLE app.items (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(64));
INSERT INTO app.items (name) VALUES ('alpha'), ('beta'), ('gamma');
CREATE USER 'dev'@'%' IDENTIFIED WITH mysql_native_password BY 'dev';
GRANT ALL PRIVILEGES ON *.* TO 'dev'@'%';
FLUSH PRIVILEGES;
SQL
mysqladmin -uroot -S /tmp/base.sock shutdown
sleep 2
umount /mnt/twig-base
EOS
SNAPID=$(aws fsx create-snapshot --region $REGION --name baseline --volume-id $BASEVOL --query 'Snapshot.SnapshotId' --output text)
while [ "$(aws fsx describe-snapshots --region $REGION --snapshot-ids $SNAPID --query 'Snapshots[0].Lifecycle' --output text)" != "AVAILABLE" ]; do sleep 3; done

# --- 4. twig 設定 + 起動(AWS 認証はローカルの一時クレデンシャルを渡す) ---
log "configure + start twigd"
aws configure export-credentials --format env > /tmp/twig-aws-env
scp -q -i $KEY /tmp/twig-aws-env ubuntu@$IP:/tmp/aws-env
rm -f /tmp/twig-aws-env
$SSH "sudo bash -s" <<EOS
set -euo pipefail
cat > /etc/twig/config.yaml <<YAML
listen:
  api: "127.0.0.1:8080"
  proxy: "0.0.0.0:3306"
state_db: /var/lib/twig/state.db
domain: twig.internal
storage:
  backend: fsx-zfs
  fsx-zfs:
    region: $REGION
    filesystem_id: $FSID
    base_volume_id: $BASEVOL
    baseline_snapshot: baseline
    dns_name: $DNS
    mount_root: /mnt/twig
engine:
  type: mysql
  mysql:
    port_range: [3401, 3410]
    env_dir: /etc/twig
    sudo: false
branches:
  name_pattern: "^[a-z0-9-]{1,32}\$"
  max_branches: 5
  lazy_create: true
  lazy_create_max_wait: 20s
hooks:
  dir: /etc/twig/hooks
  log_dir: /var/log/twig/hooks
YAML
set -a; . /tmp/aws-env; set +a
nohup /usr/local/bin/twigd --config /etc/twig/config.yaml > /var/log/twig/twigd.log 2>&1 &
sleep 2
curl -sf http://127.0.0.1:8080/v1/healthz > /dev/null
EOS

# --- 5. シナリオ ---
log "scenario: create (実測: clone+mount+mysqld で 60〜90 秒想定)"
$SSH "sudo bash -s" <<'EOS'
set -euo pipefail
q() { mysql -udev@$1 -pdev -h127.0.0.1 -P3306 -N -e "$2" 2>/dev/null; }
fail() { echo "FSX E2E FAILED: $*" >&2; tail -20 /var/log/twig/twigd.log; exit 1; }

time twig create pr-1 || fail "create"
[ "$(q pr-1 'SELECT COUNT(*) FROM app.items')" = "3" ] || fail "pr-1 should have 3 items"

echo "--- 破壊 → reset (作り直し+付け替え) ---"
q pr-1 "DELETE FROM app.items"
[ "$(q pr-1 'SELECT COUNT(*) FROM app.items')" = "0" ] || fail "delete should work"
time twig reset pr-1 || fail "reset"
[ "$(q pr-1 'SELECT COUNT(*) FROM app.items')" = "3" ] || fail "reset should restore 3 items"

echo "--- lazy create は fsx では無効(仕様 15-3) ---"
if mysql -udev@pr-lazy -pdev -h127.0.0.1 -P3306 -e "SELECT 1" 2>/dev/null; then
  fail "lazy create must be disabled on fsx"
fi

echo "--- delete (非同期・行は即消える) ---"
time twig delete pr-1 || fail "delete"
twig list | grep -q pr-1 && fail "branch should be gone from list"
echo "FSX E2E PASSED"
EOS

log "done — teardown runs via trap"
