#!/bin/bash

err_handler() {
	ret=$?
	[ "$ret" -eq 0 ] && return

	msg_error "Running ${FUNCNAME[1]} failed!"
	msg_error "exit code $ret from line ${BASH_LINENO[0]}:"
	msg_info "    $BASH_COMMAND"
	exit $ret
}

trap 'err_handler' ERR
# source files
src=$1
# generate image
dst=$2

if [ -z "$3" -o -z "$dst" -o ! -d "$src" ]; then
	echo "command format: $(basename $0) <source> <dest image> <partition size>"
	exit 0
fi
source_date_epoch="${SOURCE_DATE_EPOCH:-0}"
source_date_epoch_len="${#source_date_epoch}"
if ! [[ "$source_date_epoch" =~ ^[0-9]+$ ]] ||
	[ "$source_date_epoch_len" -gt 10 ] ||
	{ [ "$source_date_epoch_len" -eq 10 ] && [[ "$source_date_epoch" > "4294967295" ]]; }; then
	echo "SOURCE_DATE_EPOCH must be an unsigned 32-bit Unix timestamp: $source_date_epoch" >&2
	exit 1
fi
export SOURCE_DATE_EPOCH="$source_date_epoch"

read_ext4_le16_at() {
	local image="$1"
	local byte_offset="$2"
	local bytes=()

	read -r -a bytes < <(dd if="$image" bs=1 skip="$byte_offset" count=2 2>/dev/null | od -An -tu1 -v)
	if [ "${#bytes[@]}" -ne 2 ]; then
		echo "Failed to read ext4 field at byte offset $byte_offset from $image" >&2
		exit 1
	fi

	echo $((bytes[0] | (bytes[1] << 8)))
}

read_ext4_le32_at() {
	local image="$1"
	local byte_offset="$2"
	local bytes=()

	read -r -a bytes < <(dd if="$image" bs=1 skip="$byte_offset" count=4 2>/dev/null | od -An -tu1 -v)
	if [ "${#bytes[@]}" -ne 4 ]; then
		echo "Failed to read ext4 field at byte offset $byte_offset from $image" >&2
		exit 1
	fi

	echo $((bytes[0] | (bytes[1] << 8) | (bytes[2] << 16) | (bytes[3] << 24)))
}

write_ext4_le32_at() {
	local image="$1"
	local byte_offset="$2"
	local value="$3"
	local b0 b1 b2 b3

	b0=$((value & 255))
	b1=$(((value >> 8) & 255))
	b2=$(((value >> 16) & 255))
	b3=$(((value >> 24) & 255))
	printf "\\$(printf '%03o' "$b0")\\$(printf '%03o' "$b1")\\$(printf '%03o' "$b2")\\$(printf '%03o' "$b3")" |
		dd of="$image" bs=1 seek="$byte_offset" conv=notrunc >/dev/null 2>&1
}

is_ext4_superblock_at() {
	local image="$1"
	local sb_offset="$2"
	local expected_log_block_size="$3"
	local expected_blocks_per_group="$4"
	local expected_first_data_block="$5"
	local magic log_block_size blocks_per_group first_data_block

	magic="$(read_ext4_le16_at "$image" "$((sb_offset + 56))")"
	[ "$magic" -eq 61267 ] || return 1

	log_block_size="$(read_ext4_le32_at "$image" "$((sb_offset + 24))")"
	blocks_per_group="$(read_ext4_le32_at "$image" "$((sb_offset + 32))")"
	first_data_block="$(read_ext4_le32_at "$image" "$((sb_offset + 20))")"

	[ "$log_block_size" -eq "$expected_log_block_size" ] || return 1
	[ "$blocks_per_group" -eq "$expected_blocks_per_group" ] || return 1
	[ "$first_data_block" -eq "$expected_first_data_block" ] || return 1
}

normalize_ext4_superblock_times() {
	local image="$dst"
	local blocks_count first_data_block log_block_size block_size blocks_per_group
	local group_count group sb_offset patched

	blocks_count="$(read_ext4_le32_at "$image" $((1024 + 4)))"
	first_data_block="$(read_ext4_le32_at "$image" $((1024 + 20)))"
	log_block_size="$(read_ext4_le32_at "$image" $((1024 + 24)))"
	blocks_per_group="$(read_ext4_le32_at "$image" $((1024 + 32)))"

	if [ "$blocks_count" -le "$first_data_block" ] || [ "$blocks_per_group" -eq 0 ]; then
		echo "Invalid ext4 layout in $image" >&2
		exit 1
	fi

	block_size=$((1024 << log_block_size))
	group_count=$(((blocks_count - first_data_block + blocks_per_group - 1) / blocks_per_group))
	patched=0

	for ((group = 0; group < group_count; group++)); do
		if [ "$group" -eq 0 ]; then
			sb_offset=1024
		else
			sb_offset=$(((first_data_block + group * blocks_per_group) * block_size))
		fi

		if is_ext4_superblock_at "$image" "$sb_offset" "$log_block_size" "$blocks_per_group" "$first_data_block"; then
			write_ext4_le32_at "$image" "$((sb_offset + 44))" "$source_date_epoch"
			write_ext4_le32_at "$image" "$((sb_offset + 48))" "$source_date_epoch"
			write_ext4_le32_at "$image" "$((sb_offset + 64))" "$source_date_epoch"
			write_ext4_le32_at "$image" "$((sb_offset + 236))" 0
			write_ext4_le32_at "$image" "$((sb_offset + 240))" 0
			write_ext4_le32_at "$image" "$((sb_offset + 244))" 0
			write_ext4_le32_at "$image" "$((sb_offset + 248))" 0
			write_ext4_le32_at "$image" "$((sb_offset + 264))" "$source_date_epoch"
			patched=$((patched + 1))
		fi
	done

	if [ "$patched" -eq 0 ]; then
		echo "No ext4 superblocks found in $image" >&2
		exit 1
	fi

	echo "patched $patched ext4 superblock metadata copies"
}

normalize_ext4_inode_metadata() {
	local image="$dst"

	if ! command -v perl >/dev/null 2>&1; then
		echo "perl is required to normalize ext4 inode metadata" >&2
		exit 1
	fi

	perl - "$image" "$source_date_epoch" <<'PERL'
use strict;
use warnings;

my ($image, $epoch) = @ARGV;
open(my $fh, '+<:raw', $image) or die "Failed to open $image: $!\n";

sub read_at {
	my ($fh, $offset, $length) = @_;
	seek($fh, $offset, 0) or die "Failed to seek to $offset: $!\n";
	my $data = '';
	my $read = read($fh, $data, $length);
	die "Failed to read at $offset: $!\n" unless defined $read;
	die "Short read at $offset from $image\n" unless $read == $length;
	return $data;
}

sub write_at {
	my ($fh, $offset, $data) = @_;
	seek($fh, $offset, 0) or die "Failed to seek to $offset: $!\n";
	my $written = print {$fh} $data;
	die "Failed to write at $offset: $!\n" unless $written;
}

sub le16_at {
	my ($offset) = @_;
	return unpack('v', read_at($fh, $offset, 2));
}

sub le32_at {
	my ($offset) = @_;
	return unpack('V', read_at($fh, $offset, 4));
}

my $sb = 1024;
my $inodes_count = le32_at($sb);
my $first_data_block = le32_at($sb + 20);
my $log_block_size = le32_at($sb + 24);
my $inodes_per_group = le32_at($sb + 40);
my $inode_size = le16_at($sb + 88);
my $desc_size = le16_at($sb + 254);

die "Invalid ext4 inode layout in $image\n"
	if $inodes_count == 0 || $inodes_per_group == 0 || $inode_size < 128;

$desc_size = 32 if $desc_size < 32;
my $block_size = 1024 << $log_block_size;
my $group_count = int(($inodes_count + $inodes_per_group - 1) / $inodes_per_group);
my $gd_table_offset = ($first_data_block + 1) * $block_size;
my $time = pack('V', $epoch);
my $extra_time = ("\0" x 12) . $time . ("\0" x 4);
my $patched = 0;

for (my $group = 0; $group < $group_count; $group++) {
	my $group_inodes = $inodes_count - $group * $inodes_per_group;
	$group_inodes = $inodes_per_group if $group_inodes > $inodes_per_group;
	my $gd_offset = $gd_table_offset + $group * $desc_size;
	my $gd = read_at($fh, $gd_offset, $desc_size);
	my $inode_bitmap_block = unpack('V', substr($gd, 4, 4));
	my $inode_table_block = unpack('V', substr($gd, 8, 4));
	my $bitmap_bytes = int(($group_inodes + 7) / 8);
	my $bitmap = read_at($fh, $inode_bitmap_block * $block_size, $bitmap_bytes);

	for (my $inode_in_group = 0; $inode_in_group < $group_inodes; $inode_in_group++) {
		my $byte = ord(substr($bitmap, int($inode_in_group / 8), 1));
		next unless $byte & (1 << ($inode_in_group % 8));

		my $inode_offset = $inode_table_block * $block_size + $inode_in_group * $inode_size;
		write_at($fh, $inode_offset + 8, $time x 3);
		write_at($fh, $inode_offset + 100, "\0" x 4);
		write_at($fh, $inode_offset + 132, $extra_time) if $inode_size >= 152;
		$patched++;
	}
}

die "No used ext4 inodes found in $image\n" if $patched == 0;
print "patched $patched ext4 inode metadata copies\n";
PERL
}

mkfs_ext4_uuid_opt=()
case "$(basename "$dst")" in
	rootfs*.img | system*.img)
		mkfs_ext4_uuid_opt=(-U "${AIDEN_EXT4_UUID:-00000000-0000-4000-8000-000000000000}")
		;;
esac


# the size of generate image, get info from parameter.txt
# eg. 0x00040000@0x00016000(rootfs)
# calculate size fo rootfs partition: 0x00040000 * 512 = 128*0x100000 (Bytes)
dst_size="$(( $3 / 1024 / 1024 ))M"

cwd=$(dirname $(readlink -f $0))
export PATH=$cwd:$PATH

rm -f $dst

bin_dir=./
if [ -f $cwd/bin/mkfs.ext4 ];then
	bin_dir=bin
fi

mkfs_ext4_supported_disable_features() {
	local probe feature selected_features=()

	probe="$(mktemp)"
	truncate -s 1M "$probe"
	for feature in "$@"; do
		if mkfs.ext4 -q -n -O "$feature" "$probe" >/dev/null 2>&1; then
			selected_features+=("$feature")
		else
			echo "skip unsupported ext4 feature toggle: $feature" >&2
		fi
	done
	rm -f "$probe"

	local IFS=,
	echo "${selected_features[*]}"
}

mkfs_ext4_feature_opts="$(mkfs_ext4_supported_disable_features \
	^64bit \
	^huge_file \
	^metadata_csum \
	^metadata_csum_seed \
	^dir_index \
	^orphan_file \
	^quota)"

mkfs_ext4_opts=(
	-d "$src"
	-r 1
	-N 0
	-m 5
	-L ""
	"${mkfs_ext4_uuid_opt[@]}"
	-E lazy_itable_init=0,lazy_journal_init=0,root_owner=0:0
	# Avoid HTREE directories depending on mke2fs's random s_hash_seed.
	-O "$mkfs_ext4_feature_opts"
)


mkdir -p $(dirname $dst)

echo mkfs.ext4 "${mkfs_ext4_opts[@]}" "$dst" "$dst_size"
mkfs.ext4 "${mkfs_ext4_opts[@]}" "$dst" "$dst_size"
if [ $? != 0 ]; then
	echo "*** Maybe you need to increase the filesystem size "
	exit 1
fi
echo "resize2fs -M $dst"
resize2fs -M $dst
echo "e2fsck -fy  $dst"
e2fsck -fy  $dst
echo "tune2fs -m 5  $dst"
tune2fs -m 5  $dst
echo "resize2fs -M $dst"
resize2fs -M $dst
echo "normalize ext4 superblock metadata to SOURCE_DATE_EPOCH=$source_date_epoch"
normalize_ext4_superblock_times
echo "normalize ext4 inode metadata to SOURCE_DATE_EPOCH=$source_date_epoch"
normalize_ext4_inode_metadata
