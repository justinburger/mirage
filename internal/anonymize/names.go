package anonymize

import "strings"

// Embedded name lists used by the "name" and "email" fakes. They are short
// lists of very common given names and surnames, so a faked person is
// plausible but is not meant to identify anyone.

var firstNames = []string{
	"James", "Mary", "Robert", "Patricia", "John", "Jennifer", "Michael", "Linda",
	"David", "Elizabeth", "William", "Barbara", "Richard", "Susan", "Joseph", "Jessica",
	"Thomas", "Sarah", "Christopher", "Karen", "Charles", "Lisa", "Daniel", "Nancy",
	"Matthew", "Betty", "Anthony", "Margaret", "Mark", "Sandra", "Donald", "Ashley",
	"Steven", "Kimberly", "Paul", "Emily", "Andrew", "Donna", "Joshua", "Michelle",
	"Kenneth", "Carol", "Kevin", "Amanda", "Brian", "Melissa", "George", "Deborah",
	"Timothy", "Stephanie", "Ronald", "Dorothy", "Jason", "Rebecca", "Edward", "Sharon",
	"Jeffrey", "Laura", "Ryan", "Cynthia", "Jacob", "Amy", "Gary", "Kathleen",
	"Nicholas", "Angela", "Eric", "Shirley", "Jonathan", "Brenda", "Stephen", "Emma",
	"Larry", "Anna", "Justin", "Pamela", "Scott", "Nicole", "Brandon", "Samantha",
	"Benjamin", "Katherine", "Samuel", "Christine", "Gregory", "Helen", "Alexander", "Debra",
	"Patrick", "Rachel", "Frank", "Carolyn", "Raymond", "Janet", "Jack", "Maria",
	"Dennis", "Olivia", "Jerry", "Heather", "Tyler", "Diane", "Aaron", "Julie",
	"Jose", "Joyce", "Adam", "Victoria", "Nathan", "Kelly", "Henry", "Christina",
	"Zachary", "Lauren", "Douglas", "Joan", "Peter", "Evelyn", "Kyle", "Judith",
	"Noah", "Megan", "Ethan", "Cheryl", "Jeremy", "Andrea", "Walter", "Hannah",
	"Christian", "Martha", "Keith", "Jacqueline", "Roger", "Frances", "Terry", "Gloria",
	"Austin", "Ann", "Sean", "Teresa", "Gerald", "Kathryn", "Carl", "Sara",
	"Harold", "Janice", "Dylan", "Jean", "Arthur", "Alice", "Lawrence", "Madison",
	"Jordan", "Doris", "Jesse", "Abigail", "Bryan", "Julia", "Billy", "Judy",
	"Bruce", "Grace", "Gabriel", "Denise", "Joe", "Amber", "Logan", "Marilyn",
	"Alan", "Beverly", "Juan", "Danielle", "Albert", "Theresa", "Willie", "Sophia",
	"Elijah", "Marie", "Wayne", "Diana", "Randy", "Brittany", "Vincent", "Natalie",
	"Mason", "Isabella", "Roy", "Charlotte", "Ralph", "Rose", "Bobby", "Alexis",
	"Russell", "Kayla",
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis",
	"Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson", "Thomas",
	"Taylor", "Moore", "Jackson", "Martin", "Lee", "Perez", "Thompson", "White",
	"Harris", "Sanchez", "Clark", "Ramirez", "Lewis", "Robinson", "Walker", "Young",
	"Allen", "King", "Wright", "Scott", "Torres", "Nguyen", "Hill", "Flores",
	"Green", "Adams", "Nelson", "Baker", "Hall", "Rivera", "Campbell", "Mitchell",
	"Carter", "Roberts", "Gomez", "Phillips", "Evans", "Turner", "Diaz", "Parker",
	"Cruz", "Edwards", "Collins", "Reyes", "Stewart", "Morris", "Morales", "Murphy",
	"Cook", "Rogers", "Gutierrez", "Ortiz", "Morgan", "Cooper", "Peterson", "Bailey",
	"Reed", "Kelly", "Howard", "Ramos", "Kim", "Cox", "Ward", "Richardson",
	"Watson", "Brooks", "Chavez", "Wood", "James", "Bennett", "Gray", "Mendoza",
	"Ruiz", "Hughes", "Price", "Alvarez", "Castillo", "Sanders", "Patel", "Myers",
	"Long", "Ross", "Foster", "Jimenez", "Powell", "Jenkins", "Perry", "Russell",
	"Sullivan", "Bell", "Coleman", "Butler", "Henderson", "Barnes", "Fisher", "Vasquez",
	"Simmons", "Romero", "Patterson", "Alexander", "Hamilton", "Graham", "Reynolds", "Griffin",
	"Wallace", "Moreno", "West", "Cole", "Hayes", "Bryant", "Herrera", "Gibson",
	"Ellis", "Tran", "Medina", "Aguilar", "Stevens", "Murray", "Ford", "Castro",
	"Marshall", "Owens", "Harrison", "Fernandez", "Woods", "Washington", "Kennedy", "Wells",
	"Vargas", "Henry", "Chen", "Freeman", "Webb", "Tucker", "Guzman", "Burns",
	"Crawford", "Olson", "Simpson", "Porter", "Hunter", "Gordon", "Mendez", "Silva",
	"Shaw", "Snyder", "Dixon", "Munoz", "Hunt", "Hicks", "Holmes", "Palmer",
	"Wagner", "Black", "Robertson", "Boyd", "Stone", "Salazar", "Fox", "Warren",
	"Mills", "Meyer", "Rice", "Schmidt", "Garza", "Daniels", "Ferguson", "Nichols",
	"Stephens", "Soto", "Weaver", "Ryan", "Gardner", "Payne", "Grant", "Dunn",
	"Spencer", "Hawkins",
}

// emailDomains are the only domains faked addresses use. All three are
// reserved by RFC 2606, so no fake address can reach a real mailbox.
var emailDomains = [...]string{"example.com", "example.net", "example.org"}

// Lower-case copies for e-mail local parts, built once at start-up and never
// modified afterwards.
var firstNamesLower, lastNamesLower []string

func init() {
	firstNamesLower = make([]string, len(firstNames))
	for i, n := range firstNames {
		firstNamesLower[i] = strings.ToLower(n)
	}
	lastNamesLower = make([]string, len(lastNames))
	for i, n := range lastNames {
		lastNamesLower[i] = strings.ToLower(n)
	}
}
