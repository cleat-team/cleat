#!/usr/bin/perl
# Prints the packages a restored project RESOLVED, one per line: <id>\t<version>\t<path under ~/.nuget/packages>, sorted, unique.
# Read by build.sh inside the SDK container (perl and its core JSON::PP are in the image; jq and python are not).
#
#   resolved-packages.pl <path to project.assets.json>
#
# WHAT COUNTS: every `libraries` entry of type "package" (the ones a build links against, transitive ones included), and every
# `downloadDependencies` entry (packs the SDK downloads but does not link, e.g. a targeting or runtime pack). A "project" library is
# not a package and is skipped. This is exactly what `dotnet build --no-restore` reads, so it is the set whose pins matter.
# It is NOT "every directory in the cache": the cache volume is shared by every build with the same lock, and one project's unpinned
# package left in it must not fail an unrelated project (cleat#4099).
#
# Exit 2 with a message on stderr if the file is missing or is not the JSON of an assets file, so a caller cannot read "nothing printed"
# as "nothing resolved".
use strict;
use warnings;
use JSON::PP;

my $file = shift @ARGV or do { print STDERR "usage: resolved-packages.pl <project.assets.json>\n"; exit 2 };
my $json = do {
    local $/;
    open(my $fh, "<", $file) or do { print STDERR "cannot open $file: $!\n"; exit 2 };
    <$fh>;
};
my $a = eval { decode_json($json) };
if (!$a || ref $a ne "HASH" || ref($a->{libraries}) ne "HASH") {
    print STDERR "$file is not a NuGet assets file (no libraries object)\n";
    exit 2;
}
my %seen;
for my $key (keys %{ $a->{libraries} }) {
    my $lib = $a->{libraries}{$key};
    next unless ref $lib eq "HASH" && ($lib->{type} // "") eq "package";
    my $path = $lib->{path};
    if (!defined $path || $path !~ m{^([^/]+)/([^/]+)$}) {
        print STDERR "library $key has no usable path ($path)\n";
        exit 2;
    }
    $seen{"$1\t$2\t$path"} = 1;
}
for my $d (@{ $a->{downloadDependencies} // [] }) {
    my ($name, $range) = ($d->{name}, $d->{version});
    next unless defined $name && defined $range;
    # a pinned download is written "[x, x]"; the version is the first token.
    my ($v) = $range =~ /^\[?\s*([^,\s\]]+)/ or next;
    my ($id, $ver) = (lc $name, lc $v);
    $seen{"$id\t$ver\t$id/$ver"} = 1;
}
print "$_\n" for sort keys %seen;
