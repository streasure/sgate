package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"sync"

	"github.com/streasure/sgate/gateway/protobuf"
)

// Compressor 压缩管理器
type Compressor struct {
	gzipWriters sync.Pool
	gzipReaders sync.Pool
}

// NewCompressor 创建压缩管理器
func NewCompressor() *Compressor {
	return &Compressor{
		gzipWriters: sync.Pool{
			New: func() interface{} {
				return gzip.NewWriter(nil)
			},
		},
		gzipReaders: sync.Pool{
			New: func() interface{} {
				r, _ := gzip.NewReader(nil)
				return r
			},
		},
	}
}

// Compress 压缩数据
func (c *Compressor) Compress(data []byte, compressionType protobuf.CompressionType) ([]byte, int, int, error) {
	switch compressionType {
	case protobuf.CompressionType_COMPRESSION_TYPE_NONE:
		return data, len(data), len(data), nil
	case protobuf.CompressionType_COMPRESSION_TYPE_GZIP:
		return c.compressGzip(data)
	case protobuf.CompressionType_COMPRESSION_TYPE_ZSTD:
		// 暂时不支持ZSTD，使用GZIP
		return c.compressGzip(data)
	case protobuf.CompressionType_COMPRESSION_TYPE_LZ4:
		// 暂时不支持LZ4，返回未压缩数据
		return data, len(data), len(data), nil
	default:
		return data, len(data), len(data), nil
	}
}

// Decompress 解压缩数据
func (c *Compressor) Decompress(data []byte, compressionType protobuf.CompressionType) ([]byte, error) {
	switch compressionType {
	case protobuf.CompressionType_COMPRESSION_TYPE_NONE:
		return data, nil
	case protobuf.CompressionType_COMPRESSION_TYPE_GZIP:
		return c.decompressGzip(data)
	case protobuf.CompressionType_COMPRESSION_TYPE_ZSTD:
		// 暂时不支持ZSTD，返回原始数据
		return data, nil
	case protobuf.CompressionType_COMPRESSION_TYPE_LZ4:
		// 暂时不支持LZ4，返回原始数据
		return data, nil
	default:
		return data, nil
	}
}

// compressGzip 使用 GZIP 压缩数据
func (c *Compressor) compressGzip(data []byte) ([]byte, int, int, error) {
	originalSize := len(data)

	// 从对象池获取 GZIP 写入器
	w := c.gzipWriters.Get().(*gzip.Writer)
	defer c.gzipWriters.Put(w)

	// 创建缓冲区
	var buf bytes.Buffer
	w.Reset(&buf)

	// 压缩数据
	if _, err := w.Write(data); err != nil {
		return nil, 0, 0, err
	}

	// 刷新并关闭
	if err := w.Close(); err != nil {
		return nil, 0, 0, err
	}

	compressedData := buf.Bytes()
	return compressedData, originalSize, len(compressedData), nil
}

// decompressGzip 使用 GZIP 解压缩数据
func (c *Compressor) decompressGzip(data []byte) ([]byte, error) {
	// 从对象池获取 GZIP 读取器
	r := c.gzipReaders.Get().(*gzip.Reader)
	defer c.gzipReaders.Put(r)

	// 重置读取器
	if err := r.Reset(bytes.NewReader(data)); err != nil {
		return nil, err
	}

	// 读取解压后的数据
	decompressedData, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	return decompressedData, nil
}

// ShouldCompress 判断是否应该压缩
func (c *Compressor) ShouldCompress(data []byte, threshold int) bool {
	return len(data) > threshold
}

// GetCompressionType 根据数据大小选择压缩类型
func (c *Compressor) GetCompressionType(data []byte, threshold int) protobuf.CompressionType {
	if !c.ShouldCompress(data, threshold) {
		return protobuf.CompressionType_COMPRESSION_TYPE_NONE
	}

	// 只使用GZIP
	return protobuf.CompressionType_COMPRESSION_TYPE_GZIP
}

// Close 关闭压缩管理器
func (c *Compressor) Close() error {
	return nil
}
