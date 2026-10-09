# J2KGo [![PkgGoDev](https://pkg.go.dev/badge/github.com/xiaoqidun/j2kgo)](https://pkg.go.dev/github.com/xiaoqidun/j2kgo)
高性能、零依赖、纯 Go 语言 J2K 图像编解码库

# 安装指南
```shell
go get -u github.com/xiaoqidun/j2kgo
```

# 应用案例
[OFDGo](https://github.com/xiaoqidun/ofdgo)：使用 J2KGo 编解码 OFD 文档中的图像

[PDFGo](https://github.com/xiaoqidun/pdfgo)：使用 J2KGo 编解码 PDF 文档中的图像

# 解码图像
```go
package main

import (
	"image/png"
	"log"
	"os"

	"github.com/xiaoqidun/j2kgo"
)

func main() {
	file, err := os.Open("test.jp2")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	img, err := j2kgo.Decode(file)
	if err != nil {
		log.Fatal(err)
	}
	out, err := os.Create("test.png")
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(out, img); err != nil {
		out.Close()
		log.Fatal(err)
	}
	if err := out.Close(); err != nil {
		log.Fatal(err)
	}
}
```

# 标准用法
```go
package main

import (
	"image"
	"image/png"
	"log"
	"os"

	_ "github.com/xiaoqidun/j2kgo"
)

func main() {
	file, err := os.Open("test.jp2")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	img, _, err := image.Decode(file)
	if err != nil {
		log.Fatal(err)
	}
	out, err := os.Create("test.png")
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(out, img); err != nil {
		out.Close()
		log.Fatal(err)
	}
	if err := out.Close(); err != nil {
		log.Fatal(err)
	}
}
```

# 编码图像
```go
package main

import (
	"image/png"
	"log"
	"os"

	"github.com/xiaoqidun/j2kgo"
)

func main() {
	file, err := os.Open("test.png")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	img, err := png.Decode(file)
	if err != nil {
		log.Fatal(err)
	}
	out, err := os.Create("test.jp2")
	if err != nil {
		log.Fatal(err)
	}
	if err := j2kgo.Encode(out, img, nil); err != nil {
		out.Close()
		log.Fatal(err)
	}
	if err := out.Close(); err != nil {
		log.Fatal(err)
	}
}
```

# 授权协议
本项目使用 [Apache License 2.0](https://github.com/xiaoqidun/j2kgo/blob/main/LICENSE) 授权协议
