/*
 * crypto_openssl.c - minimal OpenSSL *synchronous* crypto engine for VPP
 *
 * 背景 / Rationale
 * ----------------
 * packagecloud.io/fdio 提供的 VPP 26.06-release 只打包了 crypto_sw_scheduler
 * （纯 *异步* 引擎）。`vppctl show crypto handlers` 中所有算法的 Simple/Chained
 * 两列均为空，即整个运行时没有任何“同步”crypto handler。
 *
 * WireGuard 插件（noise 握手 + 数据面）通过
 *     vnet_crypto_ctx_create(VNET_CRYPTO_ALG_CHACHA20_POLY1305[_ICV16_AAD0])
 *     vnet_crypto_process_ops(...)              <-- 同步调用路径
 * 完成 ChaCha20-Poly1305 AEAD 加解密。由于没有 SIMPLE handler，
 * vnet_crypto_process_ops_one_batch() 里 batch_fn == 0，op 状态被置为
 * FAIL_NO_HANDLER 且输出缓冲区不被写入，握手报文的 encrypted_static /
 * encrypted_timestamp 字段全是零，对端解密失败 -> "Peer error"，握手永远起不来。
 *
 * 本插件用 OpenSSL EVP 直接实现 ChaCha20-Poly1305 的 SIMPLE(同步) handler，
 * 注册为一个名为 "openssl" 的 crypto engine，补齐这块缺口。
 * 只做 WireGuard 需要的算法，保持最小化。
 *
 * 该实现针对 VPP 26.06 的 crypto API（vnet_crypto_register_ops_handler_inline +
 * vnet_crypto_sync_op_fn_t(ops[], chunks, n_ops, thread_index) + op->ctx/op->auth）
 * 编写，与旧版 24.10 的 crypto_openssl（op->key_index/op->tag）不兼容。
 */

#include <openssl/evp.h>

#include <vlib/vlib.h>
#include <vnet/plugin/plugin.h>
#include <vnet/crypto/crypto.h>
#include <vpp/app/version.h>

/*
 * vnet_crypto_register_ops_handler_inline() 由 libvnet 导出（nm -D 可见），
 * 但 26.06 的 crypto.h 未声明它，这里按 libvnet 中的实际签名补充声明。
 */
extern void vnet_crypto_register_ops_handler_inline (
  vlib_main_t *vm, vnet_crypto_engine_id_t engine, vnet_crypto_alg_t alg,
  vnet_crypto_op_type_t type, vnet_crypto_sync_op_fn_t *fn,
  vnet_crypto_sync_op_fn_t *cfn);

#define OPENSSL_SYNC_MAX_THREADS 256

/* 每线程复用的 EVP 上下文，按 thread_index 索引，无需加锁 */
static EVP_CIPHER_CTX *openssl_sync_enc_ctx[OPENSSL_SYNC_MAX_THREADS];
static EVP_CIPHER_CTX *openssl_sync_dec_ctx[OPENSSL_SYNC_MAX_THREADS];

static_always_inline EVP_CIPHER_CTX *
openssl_sync_get_ctx (EVP_CIPHER_CTX **arr, clib_thread_index_t ti)
{
  if ((u32) ti >= OPENSSL_SYNC_MAX_THREADS)
    ti = 0;
  if (PREDICT_FALSE (arr[ti] == 0))
    arr[ti] = EVP_CIPHER_CTX_new ();
  return arr[ti];
}

/* ChaCha20-Poly1305: 32B key, 12B IV(nonce), 16B tag。OpenSSL 默认 IV 长度即 12。 */

static u32
openssl_sync_chacha20_poly1305_enc (vnet_crypto_op_t *ops[],
				    vnet_crypto_op_chunk_t *chunks __clib_unused,
				    u32 n_ops, clib_thread_index_t thread_index)
{
  EVP_CIPHER_CTX *ctx = openssl_sync_get_ctx (openssl_sync_enc_ctx, thread_index);
  const EVP_CIPHER *cipher = EVP_chacha20_poly1305 ();
  u32 i;

  for (i = 0; i < n_ops; i++)
    {
      vnet_crypto_op_t *op = ops[i];
      const u8 *key = vnet_crypto_get_cipher_key (op->ctx);
      int len = 0, fin = 0;

      EVP_EncryptInit_ex (ctx, cipher, NULL, key, op->iv);
      if (op->aad_len)
	EVP_EncryptUpdate (ctx, NULL, &len, op->aad, op->aad_len);
      EVP_EncryptUpdate (ctx, op->dst, &len, op->src, op->len);
      EVP_EncryptFinal_ex (ctx, op->dst + len, &fin);
      EVP_CIPHER_CTX_ctrl (ctx, EVP_CTRL_AEAD_GET_TAG, op->auth_len, op->auth);
      op->status = VNET_CRYPTO_OP_STATUS_COMPLETED;
    }
  return n_ops;
}

static u32
openssl_sync_chacha20_poly1305_dec (vnet_crypto_op_t *ops[],
				    vnet_crypto_op_chunk_t *chunks __clib_unused,
				    u32 n_ops, clib_thread_index_t thread_index)
{
  EVP_CIPHER_CTX *ctx = openssl_sync_get_ctx (openssl_sync_dec_ctx, thread_index);
  const EVP_CIPHER *cipher = EVP_chacha20_poly1305 ();
  u32 i, n_fail = 0;

  for (i = 0; i < n_ops; i++)
    {
      vnet_crypto_op_t *op = ops[i];
      const u8 *key = vnet_crypto_get_cipher_key (op->ctx);
      int len = 0, fin = 0;

      EVP_DecryptInit_ex (ctx, cipher, NULL, key, op->iv);
      if (op->aad_len)
	EVP_DecryptUpdate (ctx, NULL, &len, op->aad, op->aad_len);
      EVP_DecryptUpdate (ctx, op->dst, &len, op->src, op->len);
      EVP_CIPHER_CTX_ctrl (ctx, EVP_CTRL_AEAD_SET_TAG, op->auth_len, op->auth);
      if (EVP_DecryptFinal_ex (ctx, op->dst + len, &fin) > 0)
	op->status = VNET_CRYPTO_OP_STATUS_COMPLETED;
      else
	{
	  n_fail++;
	  op->status = VNET_CRYPTO_OP_STATUS_FAIL_BAD_HMAC;
	}
    }
  return n_ops - n_fail;
}

static clib_error_t *
crypto_openssl_sync_init (vlib_main_t *vm)
{
  u32 eidx;

  eidx = vnet_crypto_register_engine (vm, "openssl", 100,
				      "OpenSSL Sync Engine (ChaCha20-Poly1305)");

#define register_chacha(alg)                                                 \
  do {                                                                       \
    vnet_crypto_register_ops_handler_inline (                                 \
      vm, eidx, VNET_CRYPTO_ALG_##alg, VNET_CRYPTO_OP_TYPE_ENCRYPT,           \
      openssl_sync_chacha20_poly1305_enc, 0);                                 \
    vnet_crypto_register_ops_handler_inline (                                 \
      vm, eidx, VNET_CRYPTO_ALG_##alg, VNET_CRYPTO_OP_TYPE_DECRYPT,           \
      openssl_sync_chacha20_poly1305_dec, 0);                                 \
  } while (0)

  /* 握手用（AEAD，AAD=noise hash） */
  register_chacha (CHACHA20_POLY1305);
  /* 数据面用（ICV16，AAD 长度固定变体） */
  register_chacha (CHACHA20_POLY1305_ICV16_AAD0);
  register_chacha (CHACHA20_POLY1305_ICV16_AAD8);
  register_chacha (CHACHA20_POLY1305_ICV16_AAD12);
#undef register_chacha

  (void) eidx;
  return 0;
}

VLIB_INIT_FUNCTION (crypto_openssl_sync_init) = {
  .runs_after = VLIB_INITS ("vnet_crypto_init"),
};

VLIB_PLUGIN_REGISTER () = {
  .version = VPP_BUILD_VER,
  .description = "OpenSSL Sync Crypto Engine (ChaCha20-Poly1305) for WireGuard",
};
