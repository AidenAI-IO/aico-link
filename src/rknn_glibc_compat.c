#include <ctype.h>
#include <limits.h>
#include <pthread.h>
#include <stdint.h>

/*
 * Rockchip's RV1106 mini runtime is compiled against uClibc-ng.  Its cJSON
 * parser uses the legacy uClibc data symbols instead of glibc's accessor
 * functions.  Keep the compatibility data private to this executable and
 * translate the active glibc C locale once before the first RKNN call.
 */
static uint16_t ctype_b_storage[384];
static int16_t ctype_tolower_storage[384];
static pthread_once_t ctype_once = PTHREAD_ONCE_INIT;
static int ctype_tables_valid;

const uint16_t* __ctype_b = ctype_b_storage + 128;
const int16_t* __ctype_tolower = ctype_tolower_storage + 128;

static void initialize_ctype_tables(void) {
    const unsigned short* glibc_b = *__ctype_b_loc();
    const int* glibc_tolower = *__ctype_tolower_loc();

    for (int value = -128; value <= 255; ++value) {
        const int slot = value + 128;
        const uint16_t mask = glibc_b[value];
        /* glibc stores the masks in network byte order on little-endian ARM. */
#if __BYTE_ORDER__ == __ORDER_LITTLE_ENDIAN__
        ctype_b_storage[slot] = (uint16_t)((mask >> 8) | (mask << 8));
#else
        ctype_b_storage[slot] = mask;
#endif

        int mapped = glibc_tolower[value];
        if (mapped < INT16_MIN || mapped > INT16_MAX) {
            mapped = value;
        }
        ctype_tolower_storage[slot] = (int16_t)mapped;
    }

    ctype_tables_valid =
        (__ctype_b[' '] & (1u << 5)) != 0 &&
        (__ctype_b['A'] & (1u << 0)) != 0 &&
        (__ctype_b['a'] & (1u << 1)) != 0 &&
        (__ctype_b['0'] & (1u << 3)) != 0 &&
        __ctype_tolower['A'] == 'a' &&
        __ctype_tolower['a'] == 'a';
}

int aico_rknn_glibc_compat_init(void) {
    const int ret = pthread_once(&ctype_once, initialize_ctype_tables);
    return ret == 0 && ctype_tables_valid ? 0 : -1;
}
