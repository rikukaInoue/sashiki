# Third-Party Licenses

sashiki(Apache-2.0)は以下のオープンソースモジュールを利用しており、配布する
バイナリ(`install.sh` が導入する deb 等)にはこれらがリンクされます。各モジュールは
それぞれのライセンスの下で提供されます。ソースはいずれも各モジュールの Go module
パス(`https://<module-path>`)から入手できます。

すべて permissive(Apache-2.0 / MIT / BSD-3-Clause)または weak copyleft(MPL-2.0)で、
**強いコピーレフト(GPL / AGPL / LGPL)は含みません**。sashiki 本体の Apache-2.0 と矛盾しません。

## MPL-2.0

Mozilla Public License 2.0 はファイル単位の弱いコピーレフトです。該当モジュールの
**ファイルそのものを改変**した場合のみ、その改変ファイルを MPL-2.0 で開示する義務が
生じます。依存として利用する(改変しない)分には本体の再ライセンスは不要です。
バイナリ配布にあたり、当該モジュールのソース入手経路(下記 module パス)と本表記で
MPL-2.0 の告知とします。

| モジュール | バージョン | ライセンス |
|---|---|---|
| github.com/go-sql-driver/mysql | v1.10.1 | MPL-2.0 |

## Apache-2.0

| モジュール | バージョン | ライセンス |
|---|---|---|
| github.com/aws/aws-sdk-go-v2 (config / credentials / feature/ec2/imds / internal/* / service/fsx / service/sso / service/ssooidc / service/sts / service/signin ほか) | 各 v1.x | Apache-2.0 |
| github.com/aws/smithy-go | v1.28.1 | Apache-2.0 |
| gopkg.in/yaml.v3 | v3.0.1 | Apache-2.0(一部 MIT) |

## MIT

| モジュール | バージョン | ライセンス |
|---|---|---|
| github.com/dustin/go-humanize | v1.0.1 | MIT |
| github.com/mattn/go-isatty | v0.0.24 | MIT |
| github.com/ncruces/go-strftime | v1.0.0 | MIT |

## BSD-3-Clause

| モジュール | バージョン | ライセンス |
|---|---|---|
| filippo.io/edwards25519 | v1.2.0 | BSD-3-Clause |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/remyoudompheng/bigfft | (pseudo-version) | BSD-3-Clause |
| golang.org/x/sys | v0.47.0 | BSD-3-Clause |
| modernc.org/libc | v1.75.6 | BSD-3-Clause |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause |
| modernc.org/memory | v1.12.1 | BSD-3-Clause |
| modernc.org/sqlite | v1.58.0 | BSD-3-Clause |

---

この一覧は `go version -m` で配布バイナリに実際にリンクされたモジュールから作成しています。
依存を更新したら本ファイルも更新してください。各ライセンスの全文は各モジュールの
ソースリポジトリを参照してください。
