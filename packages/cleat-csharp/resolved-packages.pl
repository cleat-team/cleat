#!/usr/bin/perl
# Two pure readers for build.sh's pin check, run inside the SDK container (perl and its core JSON::PP are in the image; jq and python are not).
#
#   resolved-packages.pl packages <project.assets.json>
#       Prints the packages ONE restored project resolved, one per line: <id>\t<version>\t<path under ~/.nuget/packages>, sorted, unique.
#       WHAT COUNTS: every `libraries` entry of type "package" (what a build links, transitive packages included) and every
#       `downloadDependencies` entry (packs the SDK downloads but does not link). A "project" library is not a package and is skipped.
#
#   resolved-packages.pl graph <output of: dotnet msbuild -getProperty:ProjectAssetsFile -getItem:ProjectReference>
#       Prints `ASSETS\t<that project's assets file>` and one `REF\t<full path>` per ProjectReference item.
#
# WHY TWO, AND WHY MSBUILD'S ITEMS RATHER THAN NUGET'S FILES (cleat#4099, cleat-review on #4104). A project reference written
# OutputItemType="Analyzer" ReferenceOutputAssembly="false" (every SDK project references its source generator that way) does NOT put the
# referenced project's packages in the referencing project's assets file, and does not always list it as a library, nor in the dgspec. Measured:
# a hello with such a reference listed nothing of the referenced project, whose package was then compiled against unchecked. MSBuild's
# ProjectReference items are the one list that names every project the build will compile, so build.sh walks those and calls `packages`
# on each project's own assets file.
#
# Both exit 2 with a message on stderr on anything they cannot read, so "printed nothing" never means "nothing resolved".
use strict;
use warnings;
use JSON::PP;

sub slurp_json {
    my ($file) = @_;
    my $json = do {
        local $/;
        open(my $fh, "<", $file) or do { print STDERR "cannot open $file: $!\n"; exit 2 };
        <$fh>;
    };
    my $d = eval { decode_json($json) };
    if (!$d || ref $d ne "HASH") { print STDERR "$file is not a JSON object\n"; exit 2 }
    return $d;
}

my ($mode, $file) = @ARGV;
if (!defined $mode || !defined $file || ($mode ne "packages" && $mode ne "graph")) {
    print STDERR "usage: resolved-packages.pl packages <project.assets.json> | graph <msbuild-eval.json>\n";
    exit 2;
}

if ($mode eq "graph") {
    my $d = slurp_json($file);
    my $assets = ($d->{Properties} // {})->{ProjectAssetsFile};
    if (!defined $assets || $assets eq "") {
        print STDERR "$file names no ProjectAssetsFile, so there is no assets file to read for this project\n";
        exit 2;
    }
    print "ASSETS\t$assets\n";
    my $refs = ($d->{Items} // {})->{ProjectReference} // [];
    if (ref $refs ne "ARRAY") { print STDERR "$file: Items.ProjectReference is not a list\n"; exit 2 }
    my %seen;
    for my $r (@$refs) {
        my $full = ref $r eq "HASH" ? $r->{FullPath} : undef;
        if (!defined $full || $full eq "") {
            print STDERR "$file: a ProjectReference item has no FullPath, so the project it names cannot be checked\n";
            exit 2;
        }
        print "REF\t$full\n" unless $seen{$full}++;
    }
    exit 0;
}

my $a = slurp_json($file);
if (ref($a->{libraries}) ne "HASH") {
    print STDERR "$file is not a NuGet assets file (no libraries object)\n";
    exit 2;
}
my %seen;
for my $key (keys %{ $a->{libraries} }) {
    my $lib = $a->{libraries}{$key};
    next unless ref $lib eq "HASH" && ($lib->{type} // "") eq "package";
    my $path = $lib->{path};
    if (!defined $path || $path !~ m{^([^/]+)/([^/]+)$}) {
        print STDERR "library $key has no usable path (" . ($path // "undef") . ")\n";
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
