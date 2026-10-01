# Third-party dependencies and UI assets

These assets are embedded into the nfs-gate binary. Their license texts are included in [third_party/licenses](third_party/licenses) and in the Docker image under `/licenses/third_party`.

| Asset | Version | Upstream | License |
| --- | --- | --- | --- |
| Tabler CSS and JS | 1.4.0 | https://github.com/tabler/tabler | MIT; `tabler.LICENSE` |
| Tabler Icons CSS and font | 3.31.0 | https://github.com/tabler/tabler-icons | MIT; `tabler-icons.LICENSE` |
| HTMX JS | 2.0.4 | https://github.com/bigskysoftware/htmx | Zero-Clause BSD; `htmx.LICENSE` |

The Tabler CSS/JS and Tabler Icons CSS retain their upstream license headers.

## S3 client dependencies

The binary also includes AWS SDK for Go v2 and Smithy Go, licensed under Apache-2.0. Their upstream LICENSE and NOTICE files are included in `third_party/licenses` and the Docker image. Exact module versions are recorded in `go.mod` and `go.sum`.

| Dependency | Upstream | License and notice |
| --- | --- | --- |
| AWS SDK for Go v2 (including config, S3 and credential providers) | https://github.com/aws/aws-sdk-go-v2 | Apache-2.0; `aws-sdk-go-v2.LICENSE`, `aws-sdk-go-v2.NOTICE` |
| Smithy Go | https://github.com/aws/smithy-go | Apache-2.0; `smithy-go.LICENSE`, `smithy-go.NOTICE` |

Go-derived code vendored inside these SDK dependencies retains its BSD-3-Clause license: `aws-singleflight.LICENSE` (AWS/Smithy singleflight) and `smithy-stdlib.LICENSE` (Smithy JSON standard-library adaptation).
