// The curated word lists a flock name is drawn from. FROZEN AND APPEND-ONLY -- see the package
// doc in flockname.go for why, and words_test.go for the assertion that enforces it.
//
// Derived from dustinkirkland/golang-petname (Apache-2.0), snapshotted 2026-08-08 at 449+449
// words and curated down to 422+424. Vendored rather than imported DELIBERATELY: upstream
// regenerates these lists as part of its release process, and an upstream word REMOVAL would
// orphan every install that had already chosen it. A dependency that is allowed to shrink
// cannot back an append-only contract, and the value here is the curation, not the code --
// the code is one modulo.
//
// What the curation dropped, and why:
//   - 27 adjectives that do not read as adjectives before an animal: prepositions and particles
//     (in, on, up), determiners (one, more, many), and nouns (boss, pet, pro, set, meet, model,
//     master, star, game, key, top, tops, mint, square, sound, still, fit, fine, first, next).
//     They produced names like `briard-up-dog.local` and `briard-boss-man.local`.
//   - 25 "names" that are not animals (man, kid, lab, imp, stud, javelin, sunbeam), are a
//     CATEGORY rather than an animal (insect, mammal, feline, primate, reptile, rodent), read
//     as their non-animal sense first (molly, racer, glider, monitor, monarch), or are
//     unsettling rather than friendly (ghost, ghoul, goblin, troll, monster, satyr, yeti).
//
// What it deliberately KEPT: `elf` and `alien` (short and good-humoured -- owner's call), and
// the five breeds `dane`, `akita`, `corgi`, `bengal`, `kodiak`, because they are real animals
// and the list keeps sixteen other breeds (labrador, poodle, beagle, collie, husky, whippet,
// mastiff, spaniel, terrier, shepherd, bulldog, malamute, doberman, airedale, foxhound,
// sheepdog) -- dropping only these five would have been arbitrary.

package flockname

var adjectives = []string{
	"able", "above", "absolute", "accepted", "accurate", "ace", "active", "actual",
	"adapted", "adapting", "adequate", "adjusted", "advanced", "alert", "alive", "allowed",
	"allowing", "amazed", "amazing", "ample", "amused", "amusing", "apparent", "apt",
	"arriving", "artistic", "assured", "assuring", "awaited", "awake", "aware", "balanced",
	"becoming", "beloved", "better", "big", "blessed", "bold", "brave", "brief",
	"bright", "bursting", "busy", "calm", "capable", "capital", "careful", "caring",
	"casual", "causal", "central", "certain", "champion", "charmed", "charming", "cheerful",
	"chief", "choice", "civil", "classic", "clean", "clear", "clever", "climbing",
	"close", "closing", "coherent", "comic", "communal", "complete", "composed", "concise",
	"concrete", "content", "cool", "correct", "cosmic", "crack", "creative", "credible",
	"crisp", "crucial", "cuddly", "cunning", "curious", "current", "cute", "daring",
	"darling", "dashing", "dear", "decent", "deciding", "deep", "definite", "delicate",
	"desired", "destined", "devoted", "direct", "discrete", "distinct", "diverse", "divine",
	"dominant", "driven", "driving", "dynamic", "eager", "easy", "electric", "elegant",
	"emerging", "eminent", "enabled", "enabling", "endless", "engaged", "engaging", "enhanced",
	"enjoyed", "enormous", "enough", "epic", "equal", "equipped", "eternal", "ethical",
	"evident", "evolved", "evolving", "exact", "excited", "exciting", "exotic", "expert",
	"factual", "fair", "faithful", "famous", "fancy", "fast", "feasible", "finer",
	"firm", "fitting", "fleet", "flexible", "flowing", "fluent", "flying", "fond",
	"frank", "free", "fresh", "full", "fun", "funky", "funny", "generous",
	"gentle", "genuine", "giving", "glad", "glorious", "glowing", "golden", "good",
	"gorgeous", "grand", "grateful", "great", "growing", "grown", "guided", "guiding",
	"handy", "happy", "hardy", "harmless", "healthy", "helped", "helpful", "helping",
	"heroic", "hip", "holy", "honest", "hopeful", "hot", "huge", "humane",
	"humble", "humorous", "ideal", "immense", "immortal", "immune", "improved", "included",
	"infinite", "informed", "innocent", "inspired", "integral", "intense", "intent", "internal",
	"intimate", "inviting", "joint", "keen", "kind", "knowing", "known", "large",
	"lasting", "leading", "learning", "legal", "legible", "lenient", "liberal", "light",
	"liked", "literate", "live", "living", "logical", "loved", "loving", "loyal",
	"lucky", "magical", "magnetic", "main", "major", "massive", "mature", "maximum",
	"measured", "merry", "mighty", "modern", "modest", "moral", "moved", "moving",
	"musical", "mutual", "national", "native", "natural", "nearby", "neat", "needed",
	"neutral", "new", "nice", "noble", "normal", "notable", "noted", "novel",
	"obliging", "open", "optimal", "optimum", "organic", "oriented", "outgoing", "patient",
	"peaceful", "perfect", "picked", "pleasant", "pleased", "pleasing", "poetic", "polished",
	"polite", "popular", "positive", "possible", "powerful", "precious", "precise", "premium",
	"prepared", "present", "pretty", "primary", "prime", "probable", "profound", "promoted",
	"prompt", "proper", "proud", "proven", "pumped", "pure", "quality", "quick",
	"quiet", "rapid", "rare", "rational", "ready", "real", "refined", "regular",
	"related", "relative", "relaxed", "relaxing", "relevant", "relieved", "renewed", "renewing",
	"resolved", "rested", "rich", "right", "robust", "romantic", "ruling", "sacred",
	"safe", "saved", "saving", "secure", "select", "selected", "sensible", "settled",
	"settling", "sharing", "sharp", "shining", "simple", "sincere", "singular", "skilled",
	"smart", "smashing", "smiling", "smooth", "social", "solid", "sought", "special",
	"splendid", "stable", "steady", "sterling", "stirred", "stirring", "striking", "strong",
	"stunning", "subtle", "suitable", "suited", "summary", "sunny", "super", "superb",
	"supreme", "sure", "sweeping", "sweet", "talented", "teaching", "tender", "thankful",
	"thorough", "tidy", "tight", "together", "tolerant", "topical", "touched", "touching",
	"tough", "true", "trusted", "trusting", "trusty", "ultimate", "unbiased", "uncommon",
	"unified", "unique", "united", "upright", "upward", "usable", "useful", "valid",
	"valued", "vast", "verified", "viable", "vital", "vocal", "wanted", "warm",
	"wealthy", "welcome", "welcomed", "well", "whole", "willing", "winning", "wired",
	"wise", "witty", "wondrous", "workable", "working", "worthy",
}

var animals = []string{
	"ox", "ant", "ape", "asp", "bat", "bee", "boa", "bug",
	"cat", "cod", "cow", "cub", "doe", "dog", "eel", "eft",
	"elf", "elk", "emu", "ewe", "fly", "fox", "gar", "gnu",
	"hen", "hog", "jay", "kit", "koi", "owl", "pig", "pug",
	"pup", "ram", "rat", "ray", "yak", "bass", "bear", "bird",
	"boar", "buck", "bull", "calf", "chow", "clam", "colt", "crab",
	"crow", "dane", "deer", "dodo", "dory", "dove", "drum", "duck",
	"fawn", "fish", "flea", "foal", "fowl", "frog", "gnat", "goat",
	"grub", "gull", "hare", "hawk", "ibex", "joey", "kite", "kiwi",
	"lamb", "lark", "lion", "loon", "lynx", "mako", "mink", "mite",
	"mole", "moth", "mule", "mutt", "newt", "orca", "oryx", "pika",
	"pony", "puma", "seal", "shad", "slug", "sole", "stag", "swan",
	"tahr", "teal", "tick", "toad", "tuna", "wasp", "wolf", "worm",
	"wren", "adder", "akita", "alien", "aphid", "bison", "boxer", "bream",
	"bunny", "burro", "camel", "chimp", "civet", "cobra", "coral", "corgi",
	"crane", "dingo", "drake", "eagle", "egret", "filly", "finch", "gator",
	"gecko", "goose", "guppy", "heron", "hippo", "horse", "hound", "husky",
	"hyena", "koala", "krill", "leech", "lemur", "liger", "llama", "louse",
	"macaw", "midge", "moose", "moray", "mouse", "panda", "perch", "prawn",
	"quail", "raven", "rhino", "robin", "shark", "sheep", "shrew", "skink",
	"skunk", "sloth", "snail", "snake", "snipe", "squid", "stork", "swift",
	"tapir", "tetra", "tiger", "trout", "viper", "wahoo", "whale", "zebra",
	"alpaca", "amoeba", "baboon", "badger", "beagle", "bedbug", "beetle", "bengal",
	"bobcat", "caiman", "cattle", "cicada", "collie", "condor", "cougar", "coyote",
	"dassie", "dragon", "earwig", "falcon", "ferret", "gannet", "gibbon", "gopher",
	"grouse", "guinea", "hermit", "hornet", "iguana", "impala", "jackal", "jaguar",
	"jennet", "kitten", "kodiak", "lizard", "locust", "maggot", "magpie", "mantis",
	"marlin", "marmot", "marten", "martin", "mayfly", "minnow", "monkey", "mullet",
	"muskox", "ocelot", "oriole", "osprey", "oyster", "parrot", "pigeon", "piglet",
	"poodle", "possum", "python", "quagga", "rabbit", "raptor", "roughy", "salmon",
	"sawfly", "serval", "shiner", "shrimp", "spider", "sponge", "tarpon", "thrush",
	"tomcat", "toucan", "turkey", "turtle", "urchin", "vervet", "walrus", "weasel",
	"weevil", "wombat", "anchovy", "anemone", "bluejay", "buffalo", "bulldog", "buzzard",
	"caribou", "catfish", "chamois", "cheetah", "chicken", "chigger", "cowbird", "crappie",
	"crawdad", "cricket", "dogfish", "dolphin", "firefly", "garfish", "gazelle", "gelding",
	"giraffe", "gobbler", "gorilla", "goshawk", "grackle", "griffon", "grizzly", "grouper",
	"haddock", "hagfish", "halibut", "hamster", "herring", "jawfish", "jaybird", "katydid",
	"ladybug", "lamprey", "lemming", "leopard", "lioness", "lobster", "macaque", "mallard",
	"mammoth", "manatee", "mastiff", "meerkat", "mollusk", "mongrel", "mudfish", "muskrat",
	"mustang", "narwhal", "oarfish", "octopus", "opossum", "ostrich", "panther", "pegasus",
	"pelican", "penguin", "phoenix", "piranha", "polecat", "quetzal", "raccoon", "rattler",
	"redbird", "redfish", "rooster", "sawfish", "sculpin", "seagull", "skylark", "snapper",
	"spaniel", "sparrow", "sunbird", "sunfish", "tadpole", "terrier", "unicorn", "vulture",
	"wallaby", "walleye", "warthog", "whippet", "wildcat", "aardvark", "airedale", "albacore",
	"anteater", "antelope", "arachnid", "barnacle", "basilisk", "blowfish", "bluebird", "bluegill",
	"bonefish", "bullfrog", "cardinal", "chipmunk", "crayfish", "dinosaur", "doberman", "duckling",
	"elephant", "escargot", "flamingo", "flounder", "foxhound", "glowworm", "goldfish", "grubworm",
	"hedgehog", "honeybee", "hookworm", "humpback", "kangaroo", "killdeer", "kingfish", "labrador",
	"lacewing", "ladybird", "lionfish", "longhorn", "mackerel", "malamute", "marmoset", "mastodon",
	"moccasin", "mongoose", "monkfish", "mosquito", "pangolin", "parakeet", "pheasant", "pipefish",
	"platypus", "polliwog", "porpoise", "reindeer", "ringtail", "sailfish", "scorpion", "seahorse",
	"seasnail", "sheepdog", "shepherd", "silkworm", "squirrel", "stallion", "starfish", "starling",
	"stingray", "stinkbug", "sturgeon", "terrapin", "titmouse", "tortoise", "treefrog", "werewolf",
}
